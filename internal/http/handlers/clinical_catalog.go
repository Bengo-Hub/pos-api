package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entdiag "github.com/bengobox/pos-service/internal/ent/diagnosiscatalog"
	entlabtest "github.com/bengobox/pos-service/internal/ent/labtest"
)

// ── Lab test catalogue ─────────────────────────────────────────────────────────

type labTestInput struct {
	Name            string  `json:"name"`
	Code            string  `json:"code,omitempty"`
	Category        string  `json:"category,omitempty"`
	Price           float64 `json:"price"`
	SampleType      string  `json:"sample_type,omitempty"`
	ReferenceRange  string  `json:"reference_range,omitempty"`
	Unit            string  `json:"unit,omitempty"`
	TurnaroundHours *int    `json:"turnaround_hours,omitempty"`
	IsActive        *bool   `json:"is_active,omitempty"`
}

// ListLabTests handles GET /{tenantID}/pos/clinical/lab-tests?category=&q=&include_inactive=
// Backs both the Settings management table and the Examination stage's orderable-test picker.
func (h *ClinicalHandler) ListLabTests(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	q := h.db.LabTest.Query().Where(entlabtest.TenantID(tid))
	if r.URL.Query().Get("include_inactive") != "true" {
		q = q.Where(entlabtest.IsActive(true))
	}
	if cat := r.URL.Query().Get("category"); cat != "" {
		q = q.Where(entlabtest.CategoryEqualFold(cat))
	}
	if search := r.URL.Query().Get("q"); search != "" {
		q = q.Where(entlabtest.Or(
			entlabtest.NameContainsFold(search),
			entlabtest.CodeContainsFold(search),
		))
	}
	rows, err := q.Order(ent.Asc(entlabtest.FieldCategory), ent.Asc(entlabtest.FieldName)).All(r.Context())
	if err != nil {
		jsonError(w, "failed to list lab tests", http.StatusInternalServerError)
		return
	}

	// Distinct categories power the Examination picker's filter chips without a second round-trip.
	catSet := map[string]struct{}{}
	for _, t := range rows {
		if t.Category != "" {
			catSet[t.Category] = struct{}{}
		}
	}
	cats := make([]string, 0, len(catSet))
	for c := range catSet {
		cats = append(cats, c)
	}
	jsonOK(w, map[string]any{"data": rows, "categories": cats})
}

// CreateLabTest handles POST /{tenantID}/pos/clinical/lab-tests
func (h *ClinicalHandler) CreateLabTest(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var in labTestInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}
	create := h.db.LabTest.Create().
		SetTenantID(tid).
		SetName(strings.TrimSpace(in.Name)).
		SetCode(in.Code).
		SetCategory(in.Category).
		SetPrice(decimal.NewFromFloat(in.Price)).
		SetSampleType(in.SampleType).
		SetReferenceRange(in.ReferenceRange).
		SetUnit(in.Unit)
	if in.TurnaroundHours != nil {
		create = create.SetTurnaroundHours(*in.TurnaroundHours)
	}
	if in.IsActive != nil {
		create = create.SetIsActive(*in.IsActive)
	}
	row, err := create.Save(r.Context())
	if err != nil {
		if ent.IsConstraintError(err) {
			jsonError(w, "a lab test with that name already exists", http.StatusConflict)
			return
		}
		h.log.Error("create lab test failed", zap.Error(err))
		jsonError(w, "failed to create lab test", http.StatusInternalServerError)
		return
	}
	jsonOK(w, row)
}

// UpdateLabTest handles PUT /{tenantID}/pos/clinical/lab-tests/{labTestID}
func (h *ClinicalHandler) UpdateLabTest(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "labTestID"))
	if err != nil {
		jsonError(w, "invalid lab_test_id", http.StatusBadRequest)
		return
	}
	existing, err := h.db.LabTest.Query().Where(entlabtest.ID(id), entlabtest.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "lab test not found", http.StatusNotFound)
		return
	}
	var in labTestInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	upd := existing.Update().
		SetCode(in.Code).
		SetCategory(in.Category).
		SetPrice(decimal.NewFromFloat(in.Price)).
		SetSampleType(in.SampleType).
		SetReferenceRange(in.ReferenceRange).
		SetUnit(in.Unit)
	if strings.TrimSpace(in.Name) != "" {
		upd = upd.SetName(strings.TrimSpace(in.Name))
	}
	if in.TurnaroundHours != nil {
		upd = upd.SetTurnaroundHours(*in.TurnaroundHours)
	}
	if in.IsActive != nil {
		upd = upd.SetIsActive(*in.IsActive)
	}
	row, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("update lab test failed", zap.Error(err))
		jsonError(w, "failed to update lab test", http.StatusInternalServerError)
		return
	}
	jsonOK(w, row)
}

// DeleteLabTest handles DELETE /{tenantID}/pos/clinical/lab-tests/{labTestID} — soft-deactivates
// rather than hard-deleting, so historical LabOrderLines keep resolving their catalogue entry.
func (h *ClinicalHandler) DeleteLabTest(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "labTestID"))
	if err != nil {
		jsonError(w, "invalid lab_test_id", http.StatusBadRequest)
		return
	}
	n, err := h.db.LabTest.Update().
		Where(entlabtest.ID(id), entlabtest.TenantID(tid)).
		SetIsActive(false).
		Save(r.Context())
	if err != nil || n == 0 {
		jsonError(w, "lab test not found", http.StatusNotFound)
		return
	}
	jsonOK(w, map[string]any{"deactivated": true})
}

// ── Diagnosis catalogue ────────────────────────────────────────────────────────

// ListDiagnoses handles GET /{tenantID}/pos/clinical/diagnoses?q=&category=
// Returns the tenant's own entries UNION the platform-curated global list, so a clinician always
// has a sensible starting set even before an admin configures anything.
func (h *ClinicalHandler) ListDiagnoses(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	q := h.db.DiagnosisCatalog.Query().Where(
		entdiag.IsActive(true),
		entdiag.Or(entdiag.TenantID(tid), entdiag.IsGlobal(true)),
	)
	if cat := r.URL.Query().Get("category"); cat != "" {
		q = q.Where(entdiag.CategoryEqualFold(cat))
	}
	if search := r.URL.Query().Get("q"); search != "" {
		q = q.Where(entdiag.Or(
			entdiag.NameContainsFold(search),
			entdiag.CodeContainsFold(search),
		))
	}
	rows, err := q.Order(ent.Asc(entdiag.FieldName)).Limit(200).All(r.Context())
	if err != nil {
		jsonError(w, "failed to list diagnoses", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": rows})
}

type diagnosisInput struct {
	Name     string `json:"name"`
	Code     string `json:"code,omitempty"`
	Category string `json:"category,omitempty"`
}

// CreateDiagnosis handles POST /{tenantID}/pos/clinical/diagnoses — used both by an admin curating
// the list and implicitly by a clinician typing a diagnosis that isn't in it yet (the catalogue
// grows organically). Idempotent on (tenant, name): re-adding an existing one returns it.
func (h *ClinicalHandler) CreateDiagnosis(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var in diagnosisInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}
	if existing, err := h.db.DiagnosisCatalog.Query().
		Where(entdiag.TenantID(tid), entdiag.NameEqualFold(name)).
		Only(r.Context()); err == nil {
		jsonOK(w, existing)
		return
	}
	row, err := h.db.DiagnosisCatalog.Create().
		SetTenantID(tid).
		SetName(name).
		SetCode(in.Code).
		SetCategory(in.Category).
		Save(r.Context())
	if err != nil {
		h.log.Error("create diagnosis failed", zap.Error(err))
		jsonError(w, "failed to create diagnosis", http.StatusInternalServerError)
		return
	}
	jsonOK(w, row)
}
