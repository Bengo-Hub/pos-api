package handlers

import (
	"encoding/json"
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entvisit "github.com/bengobox/pos-service/internal/ent/patientvisit"
	entorder "github.com/bengobox/pos-service/internal/ent/posorder"
	enttriage "github.com/bengobox/pos-service/internal/ent/triagerecord"
)

// ListVisits handles GET /{tenantID}/pos/clinical/visits?status=&outlet_id=
// Shared by every stage's "queue" view — status is whatever the caller wants (registered for
// triage, triaged for examination, etc.), kept as one generic listing endpoint.
func (h *ClinicalHandler) ListVisits(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	q := h.db.PatientVisit.Query().Where(entvisit.TenantID(tid))
	if status := r.URL.Query().Get("status"); status != "" {
		q = q.Where(entvisit.StatusEQ(entvisit.Status(status)))
	}
	if outletID := r.URL.Query().Get("outlet_id"); outletID != "" {
		if oid, err := uuid.Parse(outletID); err == nil {
			q = q.Where(entvisit.OutletID(oid))
		}
	}
	visits, err := q.Order(ent.Desc(entvisit.FieldCreatedAt)).Limit(100).All(r.Context())
	if err != nil {
		jsonError(w, "failed to list visits", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": visits})
}

// GetVisit handles GET /{tenantID}/pos/clinical/visits/{visitID} — the full clinical journey:
// patient, triage, examination, lab orders, and (if prescribed) the prescription.
func (h *ClinicalHandler) GetVisit(w http.ResponseWriter, r *http.Request) {
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
	patient, _ := h.db.Patient.Get(r.Context(), visit.PatientID)
	triage, _ := h.db.TriageRecord.Query().
		Where(enttriage.VisitID(vid)).
		Order(ent.Desc(enttriage.FieldTakenAt)).
		First(r.Context())
	examination, _ := h.latestExaminationForVisit(r, vid)
	labOrders, _ := h.labOrdersForVisit(r, vid)

	jsonOK(w, map[string]any{
		"visit": visit, "patient": patient, "triage": triage,
		"examination": examination, "lab_orders": labOrders,
	})
}

// requireRegistrationFeePaid checks the outlet's require_registration_fee toggle — when on, a
// visit cannot move into triage until its registration_fee_order is fully paid.
func (h *ClinicalHandler) requireRegistrationFeePaid(r *http.Request, outletID uuid.UUID, visit *ent.PatientVisit) (bool, string) {
	setting, err := h.db.OutletSetting.Query().Where(entoutletsetting.OutletID(outletID)).Only(r.Context())
	if err != nil || !setting.RequireRegistrationFee {
		return true, ""
	}
	if visit.RegistrationFeeOrderID == nil {
		return false, "registration fee has not been billed for this visit"
	}
	order, err := h.db.POSOrder.Query().Where(entorder.ID(*visit.RegistrationFeeOrderID)).Only(r.Context())
	if err != nil {
		return false, "registration fee order not found"
	}
	if order.PaidTotal < order.TotalAmount {
		return false, "registration fee has not been paid yet"
	}
	return true, ""
}

type triageInput struct {
	BPSystolic         *int     `json:"bp_systolic,omitempty"`
	BPDiastolic        *int     `json:"bp_diastolic,omitempty"`
	TemperatureCelsius *float64 `json:"temperature_celsius,omitempty"`
	PulseBPM           *int     `json:"pulse_bpm,omitempty"`
	RespirationRate    *int     `json:"respiration_rate,omitempty"`
	SPO2Percent        *float64 `json:"spo2_percent,omitempty"`
	WeightKg           *float64 `json:"weight_kg,omitempty"`
	HeightCm           *float64 `json:"height_cm,omitempty"`
	Notes              string   `json:"notes,omitempty"`
}

// CreateTriage handles POST /{tenantID}/pos/clinical/visits/{visitID}/triage
func (h *ClinicalHandler) CreateTriage(w http.ResponseWriter, r *http.Request) {
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
	if !h.requireModule(w, r, visit.OutletID, moduleTriage) {
		return
	}
	if ok, reason := h.requireRegistrationFeePaid(r, visit.OutletID, visit); !ok {
		jsonError(w, reason, http.StatusPaymentRequired)
		return
	}

	var input triageInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	var userID uuid.UUID
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims.Subject != "" {
		userID, _ = uuid.Parse(claims.Subject)
	}

	creator := h.db.TriageRecord.Create().
		SetTenantID(tid).
		SetVisitID(vid).
		SetTakenBy(userID).
		SetNotes(input.Notes)
	if input.BPSystolic != nil {
		creator = creator.SetBpSystolic(*input.BPSystolic)
	}
	if input.BPDiastolic != nil {
		creator = creator.SetBpDiastolic(*input.BPDiastolic)
	}
	if input.TemperatureCelsius != nil {
		creator = creator.SetTemperatureCelsius(*input.TemperatureCelsius)
	}
	if input.PulseBPM != nil {
		creator = creator.SetPulseBpm(*input.PulseBPM)
	}
	if input.RespirationRate != nil {
		creator = creator.SetRespirationRate(*input.RespirationRate)
	}
	if input.SPO2Percent != nil {
		creator = creator.SetSpo2Percent(*input.SPO2Percent)
	}
	if input.WeightKg != nil {
		creator = creator.SetWeightKg(*input.WeightKg)
	}
	if input.HeightCm != nil {
		creator = creator.SetHeightCm(*input.HeightCm)
	}

	triage, err := creator.Save(r.Context())
	if err != nil {
		h.log.Error("create triage record failed", zap.Error(err))
		jsonError(w, "failed to record vitals", http.StatusInternalServerError)
		return
	}

	updated, err := h.db.PatientVisit.UpdateOneID(vid).SetStatus(entvisit.StatusTriaged).Save(r.Context())
	if err != nil {
		h.log.Warn("failed to advance visit to triaged", zap.Error(err))
	}

	jsonOK(w, map[string]any{"triage": triage, "visit": updated})
}
