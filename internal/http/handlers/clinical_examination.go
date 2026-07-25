package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entexam "github.com/bengobox/pos-service/internal/ent/examinationrecord"
	entlabline "github.com/bengobox/pos-service/internal/ent/laborderline"
	entvisit "github.com/bengobox/pos-service/internal/ent/patientvisit"
)

func (h *ClinicalHandler) latestExaminationForVisit(r *http.Request, visitID uuid.UUID) (*ent.ExaminationRecord, error) {
	return h.db.ExaminationRecord.Query().
		Where(entexam.VisitID(visitID)).
		Order(ent.Desc(entexam.FieldExaminedAt)).
		First(r.Context())
}

type examinationInput struct {
	ChiefComplaint string   `json:"chief_complaint,omitempty"`
	Diagnosis      string   `json:"diagnosis,omitempty"`
	ClinicalNotes  string   `json:"clinical_notes,omitempty"`
	LabRequested   bool     `json:"lab_requested"`
	LabTests       []string `json:"lab_tests,omitempty"`
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

	existing, _ := h.latestExaminationForVisit(r, vid)
	var exam *ent.ExaminationRecord
	if existing != nil && existing.Status != entexam.StatusCompleted {
		upd := h.db.ExaminationRecord.UpdateOneID(existing.ID).
			SetChiefComplaint(input.ChiefComplaint).
			SetDiagnosis(input.Diagnosis).
			SetClinicalNotes(input.ClinicalNotes).
			SetLabRequested(input.LabRequested)
		exam, err = upd.Save(r.Context())
	} else {
		exam, err = h.db.ExaminationRecord.Create().
			SetTenantID(tid).
			SetVisitID(vid).
			SetExaminedBy(userID).
			SetChiefComplaint(input.ChiefComplaint).
			SetDiagnosis(input.Diagnosis).
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
	if input.LabRequested && len(input.LabTests) > 0 {
		labOrder, err = h.db.LabOrder.Create().
			SetTenantID(tid).
			SetVisitID(vid).
			SetExaminationID(exam.ID).
			SetOrderedBy(userID).
			Save(r.Context())
		if err != nil {
			h.log.Error("create lab order failed", zap.Error(err))
		} else {
			for _, test := range input.LabTests {
				if test == "" {
					continue
				}
				if _, err := h.db.LabOrderLine.Create().SetLabOrderID(labOrder.ID).SetTestName(test).Save(r.Context()); err != nil {
					h.log.Warn("create lab order line failed", zap.Error(err))
				}
			}
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
	jsonOK(w, resp)
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
