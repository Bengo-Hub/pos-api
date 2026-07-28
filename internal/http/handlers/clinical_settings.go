package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/Bengo-Hub/httpware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
)

// Pharmacy workflow configuration lives in its own endpoint rather than the generic
// /settings/modules patch: service_settings.go is already ~1150 lines (well past the
// split-at-300-400 rule), and this config is a self-contained pharmacy concern. Same
// OutletSetting row underneath — just a focused read/write surface for it.

type pharmacyWorkflowConfig struct {
	OutletID string `json:"outlet_id"`
	// direct = the prescriber also takes payment and hands over the medicine (small chemist).
	// billing = the script is posted to a shared Bills queue any cashier can settle (mid-size).
	PharmacyWorkflowMode string `json:"pharmacy_workflow_mode"`
	RequireLabPrepayment bool   `json:"require_lab_prepayment"`
}

// GetPharmacyWorkflow handles GET /{tenantID}/pos/clinical/workflow-config?outlet_id=
func (h *ClinicalHandler) GetPharmacyWorkflow(w http.ResponseWriter, r *http.Request) {
	outletID, ok := h.resolveWorkflowOutlet(w, r)
	if !ok {
		return
	}
	s, err := h.db.OutletSetting.Query().Where(entoutletsetting.OutletID(outletID)).Only(r.Context())
	if err != nil {
		// No settings row yet — report the schema defaults rather than 404ing, so the settings
		// screen renders for a freshly-created outlet.
		jsonOK(w, pharmacyWorkflowConfig{OutletID: outletID.String(), PharmacyWorkflowMode: "direct", RequireLabPrepayment: true})
		return
	}
	jsonOK(w, pharmacyWorkflowConfig{
		OutletID:             outletID.String(),
		PharmacyWorkflowMode: s.PharmacyWorkflowMode.String(),
		RequireLabPrepayment: s.RequireLabPrepayment,
	})
}

type pharmacyWorkflowPatch struct {
	OutletID             string  `json:"outlet_id"`
	PharmacyWorkflowMode *string `json:"pharmacy_workflow_mode"`
	RequireLabPrepayment *bool   `json:"require_lab_prepayment"`
}

// UpdatePharmacyWorkflow handles PATCH /{tenantID}/pos/clinical/workflow-config
func (h *ClinicalHandler) UpdatePharmacyWorkflow(w http.ResponseWriter, r *http.Request) {
	var in pharmacyWorkflowPatch
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	outletID := uuid.Nil
	if in.OutletID != "" {
		if id, err := uuid.Parse(in.OutletID); err == nil {
			outletID = id
		}
	}
	if outletID == uuid.Nil {
		var ok bool
		if outletID, ok = h.resolveWorkflowOutlet(w, r); !ok {
			return
		}
	}

	s, err := h.db.OutletSetting.Query().Where(entoutletsetting.OutletID(outletID)).Only(r.Context())
	if err != nil {
		jsonError(w, "outlet settings not found — configure the outlet first", http.StatusNotFound)
		return
	}

	upd := s.Update()
	if in.PharmacyWorkflowMode != nil {
		switch *in.PharmacyWorkflowMode {
		case "direct", "billing":
			upd = upd.SetPharmacyWorkflowMode(entoutletsetting.PharmacyWorkflowMode(*in.PharmacyWorkflowMode))
		default:
			jsonError(w, "pharmacy_workflow_mode must be 'direct' or 'billing'", http.StatusBadRequest)
			return
		}
	}
	if in.RequireLabPrepayment != nil {
		upd = upd.SetRequireLabPrepayment(*in.RequireLabPrepayment)
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("update pharmacy workflow config failed", zap.Error(err))
		jsonError(w, "failed to save pharmacy workflow settings", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pharmacyWorkflowConfig{
		OutletID:             outletID.String(),
		PharmacyWorkflowMode: updated.PharmacyWorkflowMode.String(),
		RequireLabPrepayment: updated.RequireLabPrepayment,
	})
}

// resolveWorkflowOutlet takes the outlet from ?outlet_id= when given, else the request's outlet
// context (the outlet the user is signed in to).
func (h *ClinicalHandler) resolveWorkflowOutlet(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	if raw := r.URL.Query().Get("outlet_id"); raw != "" {
		if id, err := uuid.Parse(raw); err == nil {
			return id, true
		}
	}
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if id, err := uuid.Parse(oidStr); err == nil {
			return id, true
		}
	}
	jsonError(w, "outlet_id is required", http.StatusBadRequest)
	return uuid.Nil, false
}
