package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entcsl "github.com/bengobox/pos-service/internal/ent/controlledsubstancelog"
)

// ListControlledLogs handles GET /{tenantID}/pos/pharmacy/controlled-substances
func (h *PharmacyHandler) ListControlledLogs(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.ControlledSubstanceLog.Query().
		Where(entcsl.TenantID(tid)).
		Order(ent.Desc(entcsl.FieldDispensedAt))

	if itemID := r.URL.Query().Get("catalog_item_id"); itemID != "" {
		if id, parseErr := uuid.Parse(itemID); parseErr == nil {
			q = q.Where(entcsl.CatalogItemID(id))
		}
	}

	logs, err := q.All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": logs, "total": len(logs)})
}

type createControlledLogInput struct {
	OutletID         string   `json:"outlet_id"`
	PrescriptionID   string   `json:"prescription_id,omitempty"`
	CatalogItemID    string   `json:"catalog_item_id"`
	ItemSku          string   `json:"item_sku"`
	ItemName         string   `json:"item_name"`
	QuantityDispensed float64 `json:"quantity_dispensed"`
	DispensedBy      string   `json:"dispensed_by"`
	PatientName      string   `json:"patient_name"`
	PatientIDNumber  string   `json:"patient_id_number,omitempty"`
	WitnessStaffID   string   `json:"witness_staff_id,omitempty"`
	Notes            string   `json:"notes,omitempty"`
}

// CreateControlledLog handles POST /{tenantID}/pos/pharmacy/controlled-substances
func (h *PharmacyHandler) CreateControlledLog(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createControlledLogInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.PatientName == "" || input.ItemSku == "" || input.ItemName == "" {
		jsonError(w, "patient_name, item_sku, and item_name are required", http.StatusBadRequest)
		return
	}

	outletID, err := uuid.Parse(input.OutletID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}
	itemID, err := uuid.Parse(input.CatalogItemID)
	if err != nil {
		jsonError(w, "invalid catalog_item_id", http.StatusBadRequest)
		return
	}
	dispensedBy, err := uuid.Parse(input.DispensedBy)
	if err != nil {
		jsonError(w, "invalid dispensed_by", http.StatusBadRequest)
		return
	}

	c := h.db.ControlledSubstanceLog.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetCatalogItemID(itemID).
		SetItemSku(input.ItemSku).
		SetItemName(input.ItemName).
		SetQuantityDispensed(input.QuantityDispensed).
		SetDispensedBy(dispensedBy).
		SetPatientName(input.PatientName)

	if input.PrescriptionID != "" {
		if pid, parseErr := uuid.Parse(input.PrescriptionID); parseErr == nil {
			c.SetPrescriptionID(pid)
		}
	}
	if input.PatientIDNumber != "" {
		c.SetPatientIDNumber(input.PatientIDNumber)
	}
	if input.WitnessStaffID != "" {
		if wid, parseErr := uuid.Parse(input.WitnessStaffID); parseErr == nil {
			c.SetWitnessStaffID(wid)
		}
	}
	if input.Notes != "" {
		c.SetNotes(input.Notes)
	}

	logEntry, err := c.Save(r.Context())
	if err != nil {
		h.log.Error("create controlled substance log failed", zap.Error(err))
		jsonError(w, "failed to create log entry: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, logEntry)
}

// GetControlledLog handles GET /{tenantID}/pos/pharmacy/controlled-substances/{logID}
func (h *PharmacyHandler) GetControlledLog(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	logID, err := uuid.Parse(chi.URLParam(r, "logID"))
	if err != nil {
		jsonError(w, "invalid log id", http.StatusBadRequest)
		return
	}

	entry, err := h.db.ControlledSubstanceLog.Query().
		Where(entcsl.ID(logID), entcsl.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "log entry not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, entry)
}
