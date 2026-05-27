package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entdic "github.com/bengobox/pos-service/internal/ent/druginteractioncheck"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	entpx "github.com/bengobox/pos-service/internal/ent/prescription"
	entpxl "github.com/bengobox/pos-service/internal/ent/prescriptionline"
)

type PharmacyHandler struct {
	log *zap.Logger
	db  *ent.Client
}

func NewPharmacyHandler(log *zap.Logger, db *ent.Client) *PharmacyHandler {
	return &PharmacyHandler{log: log, db: db}
}

type prescriptionLineInput struct {
	DrugName           string   `json:"drug_name"`
	Dosage             string   `json:"dosage"`
	Form               string   `json:"form"`
	Instructions       string   `json:"instructions"`
	QuantityPrescribed int      `json:"quantity_prescribed"`
	CatalogItemID      *string  `json:"catalog_item_id"`
	LotNumber          string   `json:"lot_number"`
	UnitPrice          *float64 `json:"unit_price"`
}

type createPrescriptionInput struct {
	OutletID          string                  `json:"outlet_id"`
	OrderID           *string                 `json:"order_id"`
	PrescriptionNumber string                 `json:"prescription_number"`
	PrescriberName    string                  `json:"prescriber_name"`
	PrescriberLicense string                  `json:"prescriber_license"`
	PatientName       string                  `json:"patient_name"`
	PatientDOB        string                  `json:"patient_dob"`
	PatientIDNumber   string                  `json:"patient_id_number"`
	Notes             string                  `json:"notes"`
	Lines             []prescriptionLineInput `json:"lines"`
}

type interactionCheckInput struct {
	DrugSKUs       []string `json:"drug_skus"`
	PrescriptionID *string  `json:"prescription_id"`
	OrderID        *string  `json:"order_id"`
}

// CreatePrescription handles POST /{tenantID}/pos/pharmacy/prescriptions
func (h *PharmacyHandler) CreatePrescription(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createPrescriptionInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.PrescriptionNumber == "" || input.PatientName == "" {
		jsonError(w, "prescription_number and patient_name are required", http.StatusBadRequest)
		return
	}

	outletID, err := uuid.Parse(input.OutletID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}

	creator := h.db.Prescription.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetPrescriptionNumber(input.PrescriptionNumber).
		SetPrescriberName(input.PrescriberName).
		SetPrescriberLicense(input.PrescriberLicense).
		SetPatientName(input.PatientName).
		SetPatientDob(input.PatientDOB).
		SetPatientIDNumber(input.PatientIDNumber).
		SetNotes(input.Notes)

	if input.OrderID != nil {
		oid, err := uuid.Parse(*input.OrderID)
		if err == nil {
			creator = creator.SetOrderID(oid)
		}
	}

	px, err := creator.Save(r.Context())
	if err != nil {
		h.log.Error("create prescription failed", zap.Error(err))
		jsonError(w, "failed to create prescription", http.StatusInternalServerError)
		return
	}

	for _, li := range input.Lines {
		if li.DrugName == "" || li.QuantityPrescribed <= 0 {
			continue
		}
		lc := h.db.PrescriptionLine.Create().
			SetPrescriptionID(px.ID).
			SetDrugName(li.DrugName).
			SetDosage(li.Dosage).
			SetForm(li.Form).
			SetInstructions(li.Instructions).
			SetQuantityPrescribed(li.QuantityPrescribed).
			SetLotNumber(li.LotNumber)
		if li.CatalogItemID != nil {
			cid, err := uuid.Parse(*li.CatalogItemID)
			if err == nil {
				lc = lc.SetCatalogItemID(cid)
			}
		}
		if li.UnitPrice != nil {
			lc = lc.SetUnitPrice(decimal.NewFromFloat(*li.UnitPrice))
		}
		if _, err := lc.Save(r.Context()); err != nil {
			h.log.Warn("failed to create prescription line", zap.Error(err))
		}
	}

	lines, _ := h.db.PrescriptionLine.Query().
		Where(entpxl.PrescriptionID(px.ID)).
		All(r.Context())

	jsonOK(w, map[string]any{"prescription": px, "lines": lines})
}

// ListPrescriptions handles GET /{tenantID}/pos/pharmacy/prescriptions
func (h *PharmacyHandler) ListPrescriptions(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.Prescription.Query().Where(entpx.TenantID(tid))

	if s := r.URL.Query().Get("status"); s != "" {
		q = q.Where(entpx.StatusEQ(s))
	}
	if name := r.URL.Query().Get("patient_name"); name != "" {
		q = q.Where(entpx.PatientNameContainsFold(name))
	}

	p := pagination.Parse(r)
	total, _ := q.Clone().Count(r.Context())
	prescriptions, err := q.Order(ent.Desc(entpx.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list prescriptions failed", zap.Error(err))
		jsonError(w, "failed to list prescriptions", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(prescriptions, total, p))
}

// GetPrescription handles GET /{tenantID}/pos/pharmacy/prescriptions/{id}
func (h *PharmacyHandler) GetPrescription(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	pxID, err := uuid.Parse(chi.URLParam(r, "prescriptionID"))
	if err != nil {
		jsonError(w, "invalid prescription_id", http.StatusBadRequest)
		return
	}

	px, err := h.db.Prescription.Get(r.Context(), pxID)
	if err != nil || px.TenantID != tid {
		jsonError(w, "prescription not found", http.StatusNotFound)
		return
	}

	lines, _ := h.db.PrescriptionLine.Query().
		Where(entpxl.PrescriptionID(pxID)).
		All(r.Context())

	jsonOK(w, map[string]any{"prescription": px, "lines": lines})
}

// Dispense handles POST /{tenantID}/pos/pharmacy/prescriptions/{id}/dispense
func (h *PharmacyHandler) Dispense(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	pxID, err := uuid.Parse(chi.URLParam(r, "prescriptionID"))
	if err != nil {
		jsonError(w, "invalid prescription_id", http.StatusBadRequest)
		return
	}

	px, err := h.db.Prescription.Get(r.Context(), pxID)
	if err != nil || px.TenantID != tid {
		jsonError(w, "prescription not found", http.StatusNotFound)
		return
	}

	var body struct {
		DispensedBy *string `json:"dispensed_by"`
		Notes       string  `json:"notes"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	now := time.Now()
	upd := h.db.Prescription.UpdateOneID(pxID).
		SetStatus("dispensed").
		SetDispensedAt(now)
	if body.DispensedBy != nil {
		if dbID, err := uuid.Parse(*body.DispensedBy); err == nil {
			upd = upd.SetDispensedBy(dbID)
		}
	}
	if body.Notes != "" {
		upd = upd.SetNotes(body.Notes)
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("dispense prescription failed", zap.Error(err))
		jsonError(w, "failed to dispense prescription", http.StatusInternalServerError)
		return
	}

	h.db.PrescriptionLine.Update().
		Where(entpxl.PrescriptionID(pxID), entpxl.StatusEQ("pending")).
		SetStatus("dispensed").
		SaveX(r.Context())

	jsonOK(w, updated)
}

// ListPatients handles GET /{tenantID}/pos/pharmacy/patients
// Derives distinct patient records from the prescriptions table.
func (h *PharmacyHandler) ListPatients(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.Prescription.Query().Where(entpx.TenantID(tid))
	if search := r.URL.Query().Get("q"); search != "" {
		q = q.Where(entpx.PatientNameContainsFold(search))
	}

	prescriptions, err := q.Order(ent.Desc(entpx.FieldCreatedAt)).All(r.Context())
	if err != nil {
		h.log.Error("list patients failed", zap.Error(err))
		jsonError(w, "failed to list patients", http.StatusInternalServerError)
		return
	}

	// Deduplicate by patient_id_number (fallback: patient_name)
	seen := make(map[string]struct{})
	type patientRow struct {
		PatientName     string `json:"patient_name"`
		PatientDOB      string `json:"patient_dob"`
		PatientIDNumber string `json:"patient_id_number"`
		VisitCount      int    `json:"visit_count"`
		LastVisit       string `json:"last_visit"`
	}
	countMap := make(map[string]*patientRow)
	for _, px := range prescriptions {
		key := px.PatientIDNumber
		if key == "" {
			key = px.PatientName
		}
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			countMap[key] = &patientRow{
				PatientName:     px.PatientName,
				PatientDOB:      px.PatientDob,
				PatientIDNumber: px.PatientIDNumber,
				LastVisit:       px.CreatedAt.Format(time.RFC3339),
			}
		}
		countMap[key].VisitCount++
	}

	allPatients := make([]*patientRow, 0, len(countMap))
	for _, row := range countMap {
		allPatients = append(allPatients, row)
	}

	pg := pagination.Parse(r)
	total := len(allPatients)
	start := pg.Offset
	if start > total {
		start = total
	}
	end := start + pg.Limit
	if end > total {
		end = total
	}
	jsonOK(w, pagination.NewResponse(allPatients[start:end], total, pg))
}

// CreateInteractionCheck handles POST /{tenantID}/pos/pharmacy/interaction-checks
func (h *PharmacyHandler) CreateInteractionCheck(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input interactionCheckInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(input.DrugSKUs) == 0 {
		jsonError(w, "drug_skus is required", http.StatusBadRequest)
		return
	}

	creator := h.db.DrugInteractionCheck.Create().
		SetTenantID(tid).
		SetDrugSkus(input.DrugSKUs).
		SetCheckedAt(time.Now())

	if input.PrescriptionID != nil {
		if pid, err := uuid.Parse(*input.PrescriptionID); err == nil {
			creator = creator.SetPrescriptionID(pid)
		}
	}
	if input.OrderID != nil {
		if oid, err := uuid.Parse(*input.OrderID); err == nil {
			creator = creator.SetOrderID(oid)
		}
	}

	check, err := creator.Save(r.Context())
	if err != nil {
		h.log.Error("create interaction check failed", zap.Error(err))
		jsonError(w, "failed to create interaction check", http.StatusInternalServerError)
		return
	}

	_ = entdic.TenantID(tid)
	jsonOK(w, check)
}

type ageVerifyInput struct {
	SKU         string `json:"sku"`          // item SKU to check
	CustomerDOB string `json:"customer_dob"` // YYYY-MM-DD
}

// AgeVerify handles POST /{tenantID}/pos/pharmacy/age-verify
// Returns 200 if the customer meets the item's minimum age requirement, 422 if blocked.
func (h *PharmacyHandler) AgeVerify(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input ageVerifyInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.SKU == "" || input.CustomerDOB == "" {
		jsonError(w, "sku and customer_dob are required", http.StatusBadRequest)
		return
	}

	dob, err := time.Parse("2006-01-02", input.CustomerDOB)
	if err != nil {
		jsonError(w, "customer_dob must be in YYYY-MM-DD format", http.StatusBadRequest)
		return
	}

	override, err := h.db.POSCatalogOverride.Query().
		Where(
			entoverride.TenantID(tid),
			entoverride.InventorySku(input.SKU),
			entoverride.RequiresAgeVerificationEQ(true),
			entoverride.MinimumAgeNotNil(),
		).First(r.Context())
	if err != nil {
		// No age-restricted override found — allow
		jsonOK(w, map[string]any{"allowed": true, "message": "no age restriction for this item"})
		return
	}

	minimumAge := *override.MinimumAge
	ageYears := int(time.Since(dob).Hours() / 8766) // approx years
	if ageYears < minimumAge {
		jsonError(w, "customer does not meet the minimum age requirement", http.StatusUnprocessableEntity)
		return
	}

	jsonOK(w, map[string]any{
		"allowed":      true,
		"minimum_age":  minimumAge,
		"customer_age": ageYears,
	})
}
