package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entexam "github.com/bengobox/pos-service/internal/ent/examinationrecord"
	entlaborder "github.com/bengobox/pos-service/internal/ent/laborder"
	entlabline "github.com/bengobox/pos-service/internal/ent/laborderline"
	entlabtest "github.com/bengobox/pos-service/internal/ent/labtest"
	entoutletsettingExam "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entvisit "github.com/bengobox/pos-service/internal/ent/patientvisit"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

func (h *ClinicalHandler) latestExaminationForVisit(r *http.Request, visitID uuid.UUID) (*ent.ExaminationRecord, error) {
	return h.db.ExaminationRecord.Query().
		Where(entexam.VisitID(visitID)).
		Order(ent.Desc(entexam.FieldExaminedAt)).
		First(r.Context())
}

type examinationInput struct {
	ChiefComplaint string `json:"chief_complaint,omitempty"`
	// Diagnosis is the free-text summary; DiagnosisCodes carries the structured multi-select from
	// the DiagnosisCatalog (a visit may legitimately carry a combination). When only codes are
	// sent, Diagnosis is derived by joining them so every existing reader keeps working.
	Diagnosis      string   `json:"diagnosis,omitempty"`
	DiagnosisCodes []string `json:"diagnosis_codes,omitempty"`
	ClinicalNotes  string   `json:"clinical_notes,omitempty"`
	LabRequested   bool     `json:"lab_requested"`
	// LabTestIDs are LabTest catalogue picks (priced); LabTests are free-typed one-offs kept for
	// backward compatibility and for a test the catalogue doesn't carry yet.
	LabTestIDs []string `json:"lab_test_ids,omitempty"`
	LabTests   []string `json:"lab_tests,omitempty"`
}

// CreateExamination handles POST /{tenantID}/pos/clinical/visits/{visitID}/examination
// Creates the visit's ExaminationRecord (or updates it if the examiner is refining a diagnosis
// after lab results come back). When lab_requested, also opens a LabOrder with one line per
// requested test and parks the visit at awaiting_lab.
func (h *ClinicalHandler) CreateExamination(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	vid, err := uuid.Parse(chi.URLParam(r, "visitID"))
	if err != nil {
		jsonError(w, "invalid visit_id", http.StatusBadRequest)
		return
	}
	visit, err := h.db.PatientVisit.Query().Where(entvisit.ID(vid), entvisit.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "visit not found", http.StatusNotFound)
		return
	}
	if !h.requireModule(w, r, visit.OutletID, moduleExamination) {
		return
	}

	var input examinationInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	var userID uuid.UUID
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims.Subject != "" {
		userID, _ = uuid.Parse(claims.Subject)
	}

	// A combination diagnosis is stored structurally AND as the joined human-readable summary, so
	// reports/receipts that only know about the single `diagnosis` string keep working unchanged.
	diagnosisText := strings.TrimSpace(input.Diagnosis)
	if diagnosisText == "" && len(input.DiagnosisCodes) > 0 {
		diagnosisText = strings.Join(input.DiagnosisCodes, ", ")
	}

	existing, _ := h.latestExaminationForVisit(r, vid)
	var exam *ent.ExaminationRecord
	if existing != nil && existing.Status != entexam.StatusCompleted {
		upd := h.db.ExaminationRecord.UpdateOneID(existing.ID).
			SetChiefComplaint(input.ChiefComplaint).
			SetDiagnosis(diagnosisText).
			SetDiagnosisCodes(input.DiagnosisCodes).
			SetClinicalNotes(input.ClinicalNotes).
			SetLabRequested(input.LabRequested)
		exam, err = upd.Save(r.Context())
	} else {
		exam, err = h.db.ExaminationRecord.Create().
			SetTenantID(tid).
			SetVisitID(vid).
			SetExaminedBy(userID).
			SetChiefComplaint(input.ChiefComplaint).
			SetDiagnosis(diagnosisText).
			SetDiagnosisCodes(input.DiagnosisCodes).
			SetClinicalNotes(input.ClinicalNotes).
			SetLabRequested(input.LabRequested).
			Save(r.Context())
	}
	if err != nil {
		h.log.Error("save examination record failed", zap.Error(err))
		jsonError(w, "failed to save examination", http.StatusInternalServerError)
		return
	}

	visitStatus := entvisit.StatusInExamination
	var labOrder *ent.LabOrder
	var labBill *ent.POSOrder
	if input.LabRequested && (len(input.LabTestIDs) > 0 || len(input.LabTests) > 0) {
		labOrder, labBill, err = h.createLabOrder(r, tid, vid, visit.OutletID, exam.ID, userID, input)
		if err != nil {
			h.log.Error("create lab order failed", zap.Error(err))
		} else {
			if _, err := h.db.ExaminationRecord.UpdateOneID(exam.ID).SetStatus(entexam.StatusAwaitingLab).Save(r.Context()); err == nil {
				exam.Status = entexam.StatusAwaitingLab
			}
			visitStatus = entvisit.StatusAwaitingLab
		}
	}

	updatedVisit, err := h.db.PatientVisit.UpdateOneID(vid).SetStatus(visitStatus).Save(r.Context())
	if err != nil {
		h.log.Warn("failed to advance visit status", zap.Error(err))
	}

	resp := map[string]any{"examination": exam, "visit": updatedVisit}
	if labOrder != nil {
		lines, _ := h.db.LabOrderLine.Query().Where(entlabline.LabOrderID(labOrder.ID)).All(r.Context())
		resp["lab_order"] = labOrder
		resp["lab_order_lines"] = lines
	}
	if labBill != nil {
		resp["lab_payment_order"] = map[string]any{
			"id": labBill.ID, "order_number": labBill.OrderNumber, "total_amount": labBill.TotalAmount,
		}
	}
	jsonOK(w, resp)
}

// createLabOrder builds the LabOrder + its priced lines and, when the outlet requires lab
// pre-payment, the bill the patient must settle before the Lab module will show the order. The
// bill goes through the SAME order-creation path every POS sale uses, so lab revenue lands in the
// normal GL/receipt/eTIMS pipeline rather than a parallel billing system.
func (h *ClinicalHandler) createLabOrder(
	r *http.Request, tid, visitID, outletID, examID, userID uuid.UUID, input examinationInput,
) (*ent.LabOrder, *ent.POSOrder, error) {
	// Resolve catalogue picks once (priced), then append any free-typed one-off tests.
	type pendingLine struct {
		testID *uuid.UUID
		name   string
		price  decimal.Decimal
		unit   string
		refRng string
	}
	pending := make([]pendingLine, 0, len(input.LabTestIDs)+len(input.LabTests))
	if len(input.LabTestIDs) > 0 {
		ids := make([]uuid.UUID, 0, len(input.LabTestIDs))
		for _, s := range input.LabTestIDs {
			if id, err := uuid.Parse(s); err == nil {
				ids = append(ids, id)
			}
		}
		tests, _ := h.db.LabTest.Query().Where(entlabtest.IDIn(ids...), entlabtest.TenantID(tid)).All(r.Context())
		for _, t := range tests {
			id := t.ID
			pending = append(pending, pendingLine{testID: &id, name: t.Name, price: t.Price, unit: t.Unit, refRng: t.ReferenceRange})
		}
	}
	for _, name := range input.LabTests {
		if strings.TrimSpace(name) == "" {
			continue
		}
		pending = append(pending, pendingLine{name: strings.TrimSpace(name)})
	}
	if len(pending) == 0 {
		return nil, nil, fmt.Errorf("no valid lab tests requested")
	}

	total := decimal.Zero
	for _, p := range pending {
		total = total.Add(p.price)
	}

	prepay := h.labPrepaymentRequired(r, outletID) && total.IsPositive()
	status := entlaborder.StatusOrdered
	if prepay {
		status = entlaborder.StatusAwaitingPayment
	}

	labOrder, err := h.db.LabOrder.Create().
		SetTenantID(tid).
		SetVisitID(visitID).
		SetExaminationID(examID).
		SetOrderedBy(userID).
		SetStatus(status).
		SetTotalAmount(total).
		Save(r.Context())
	if err != nil {
		return nil, nil, err
	}

	for _, p := range pending {
		lc := h.db.LabOrderLine.Create().
			SetLabOrderID(labOrder.ID).
			SetTestName(p.name).
			SetPrice(p.price).
			SetUnit(p.unit).
			SetReferenceRange(p.refRng)
		if p.testID != nil {
			lc = lc.SetLabTestID(*p.testID)
		}
		if _, err := lc.Save(r.Context()); err != nil {
			h.log.Warn("create lab order line failed", zap.Error(err))
		}
	}

	if !prepay || h.orderSvc == nil {
		return labOrder, nil, nil
	}

	// Bill: one order line per test, priced from the catalogue. Lab tests are a SERVICE — no
	// inventory backflush — so lines carry no SKU and the sale-finalize consumer skips them.
	tenantSlug := resolveTenantSlug(r, h.db)
	orderLines := make([]orders.OrderLineInput, 0, len(pending))
	for _, p := range pending {
		price, _ := p.price.Float64()
		orderLines = append(orderLines, orders.OrderLineInput{
			Name:       "Lab: " + p.name,
			Quantity:   1,
			UnitPrice:  price,
			TotalPrice: price,
			Metadata:   map[string]any{"lab_order_id": labOrder.ID.String()},
		})
	}
	bill, err := h.orderSvc.CreateOrder(r.Context(), orders.CreateOrderRequest{
		TenantID:     tid,
		TenantSlug:   tenantSlug,
		OutletID:     outletID,
		UserID:       userID,
		Lines:        orderLines,
		OrderSubtype: "retail",
		Source:       "pos_terminal",
		Metadata:     map[string]any{"lab_order_id": labOrder.ID.String(), "visit_id": visitID.String()},
	})
	if err != nil {
		h.log.Warn("lab bill creation failed — order left awaiting_payment", zap.Error(err))
		return labOrder, nil, nil
	}
	if updated, uerr := h.db.LabOrder.UpdateOneID(labOrder.ID).SetPaymentOrderID(bill.ID).Save(r.Context()); uerr == nil {
		labOrder = updated
	}
	return labOrder, bill, nil
}

// labPrepaymentRequired reads the outlet's require_lab_prepayment toggle (default ON when the
// outlet has no settings row yet — a lab that never configured anything should still collect).
func (h *ClinicalHandler) labPrepaymentRequired(r *http.Request, outletID uuid.UUID) bool {
	s, err := h.db.OutletSetting.Query().Where(entoutletsettingExam.OutletID(outletID)).Only(r.Context())
	if err != nil {
		return true
	}
	return s.RequireLabPrepayment
}

// PrescribeFromExamination handles POST /{tenantID}/pos/clinical/visits/{visitID}/prescribe
// Creates the Prescription for this visit via the SAME core path a standalone pharmacy-counter
// prescription uses (pharmacy.createPrescriptionCore) — patient identity and prescriber are
// pre-filled from the visit/examiner rather than re-typed, and the resulting Rx flows through
// the existing Approve -> Lock -> Dispense -> Checkout pharmacy pipeline unchanged.
func (h *ClinicalHandler) PrescribeFromExamination(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	vid, err := uuid.Parse(chi.URLParam(r, "visitID"))
	if err != nil {
		jsonError(w, "invalid visit_id", http.StatusBadRequest)
		return
	}
	if h.pharmacy == nil {
		jsonError(w, "pharmacy service is not available", http.StatusServiceUnavailable)
		return
	}
	visit, err := h.db.PatientVisit.Query().Where(entvisit.ID(vid), entvisit.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "visit not found", http.StatusNotFound)
		return
	}
	if !h.requireModule(w, r, visit.OutletID, moduleExamination) {
		return
	}
	patient, err := h.db.Patient.Get(r.Context(), visit.PatientID)
	if err != nil {
		jsonError(w, "patient not found", http.StatusNotFound)
		return
	}
	exam, err := h.latestExaminationForVisit(r, vid)
	if err != nil {
		jsonError(w, "run an examination before prescribing", http.StatusConflict)
		return
	}

	var body struct {
		PrescriberName    string                  `json:"prescriber_name"`
		PrescriberLicense string                  `json:"prescriber_license"`
		Notes             string                  `json:"notes,omitempty"`
		Lines             []prescriptionLineInput `json:"lines"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.PrescriberName == "" || len(body.Lines) == 0 {
		jsonError(w, "prescriber_name and at least one drug line are required", http.StatusBadRequest)
		return
	}

	dob := ""
	if patient.Dob != nil {
		dob = patient.Dob.Format("2006-01-02")
	}
	input := createPrescriptionInput{
		OutletID:          visit.OutletID.String(),
		PrescriberName:    body.PrescriberName,
		PrescriberLicense: body.PrescriberLicense,
		PatientName:       patient.FullName,
		PatientDOB:        dob,
		PatientIDNumber:   patient.IDNumber,
		Notes:             body.Notes,
		AllergyFlags:      patient.AllergyFlags,
		Lines:             body.Lines,
	}

	px, lines, err := h.pharmacy.createPrescriptionCore(r, tid, visit.OutletID, input, &patient.ID, &vid, "")
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	now := time.Now()
	if _, err := h.db.ExaminationRecord.UpdateOneID(exam.ID).
		SetStatus(entexam.StatusCompleted).
		SetPrescriptionID(px.ID).
		SetCompletedAt(now).
		Save(r.Context()); err != nil {
		h.log.Warn("failed to complete examination record", zap.Error(err))
	}
	updatedVisit, err := h.db.PatientVisit.UpdateOneID(vid).SetStatus(entvisit.StatusPrescribed).Save(r.Context())
	if err != nil {
		h.log.Warn("failed to advance visit to prescribed", zap.Error(err))
	}

	jsonOK(w, map[string]any{"prescription": px, "lines": lines, "visit": updatedVisit})
}
