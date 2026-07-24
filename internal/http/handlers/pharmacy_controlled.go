package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entcsl "github.com/bengobox/pos-service/internal/ent/controlledsubstancelog"
	entpxl "github.com/bengobox/pos-service/internal/ent/prescriptionline"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
)

// controlledSubstanceDispenseAction is the step-up action name a witness must be issued a
// token for (see canApproveAction in step_up.go — pharmacist role is witness-eligible for
// this action specifically, not for manager-only actions like void/discount override).
const controlledSubstanceDispenseAction = "controlled_substance_dispense"

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

	p := pagination.Parse(r)
	total, _ := q.Clone().Count(r.Context())
	logs, err := q.Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(logs, total, p))
}

type createControlledLogInput struct {
	OutletID          string  `json:"outlet_id"`
	PrescriptionID    string  `json:"prescription_id,omitempty"`
	CatalogItemID     string  `json:"catalog_item_id"`
	ItemSku           string  `json:"item_sku"`
	ItemName          string  `json:"item_name"`
	QuantityDispensed float64 `json:"quantity_dispensed"`
	DispensedBy       string  `json:"dispensed_by"`
	PatientName       string  `json:"patient_name"`
	PatientIDNumber   string  `json:"patient_id_number,omitempty"`
	WitnessStaffID    string  `json:"witness_staff_id,omitempty"`
	Notes             string  `json:"notes,omitempty"`
	LotNumber         string  `json:"lot_number,omitempty"`
	ApprovalToken     string  `json:"approval_token"`
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

	// Dual-person dispensing requirement: the witness must be verified via the platform's
	// existing step-up mechanism (PIN or QR manager-card), not merely named in the request
	// body. canApproveAction (step_up.go) already restricted token ISSUANCE for this action to
	// managers or pharmacists, so a valid token here is proof the approver was eligible —
	// no further role check needed.
	approverUserID, tokenOK := verifyApprovalToken(input.ApprovalToken, controlledSubstanceDispenseAction, h.terminalSecret)
	if !tokenOK {
		jsonError(w, "a valid witness approval (step-up PIN or manager card) is required to dispense a controlled substance", http.StatusForbidden)
		return
	}
	witness, werr := h.db.StaffMember.Query().
		Where(entstaff.TenantID(tid), entstaff.UserID(approverUserID)).
		Only(r.Context())
	if werr != nil {
		jsonError(w, "witness staff record not found", http.StatusForbidden)
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
		SetPatientName(input.PatientName).
		SetWitnessStaffID(witness.ID) // authoritative — from the verified token, not the request body

	lotNumber := input.LotNumber
	var lotExpiry *ent.PrescriptionLine
	if input.PrescriptionID != "" {
		if pid, parseErr := uuid.Parse(input.PrescriptionID); parseErr == nil {
			c.SetPrescriptionID(pid)
			// Batch traceability: a controlled-substance dispense almost always originates
			// from a prescription line, which already carries the lot the pharmacist filled
			// from (Sprint 8 field). Prefer the caller-supplied lot_number (explicit correction)
			// but fall back to the prescription line so this is populated automatically.
			if lotNumber == "" {
				if line, lerr := h.db.PrescriptionLine.Query().
					Where(entpxl.PrescriptionID(pid), entpxl.CatalogItemID(itemID)).
					First(r.Context()); lerr == nil && line != nil {
					lotNumber = line.LotNumber
					lotExpiry = line
				}
			}
		}
	}
	if lotNumber != "" {
		c.SetLotNumber(lotNumber)
	}
	if lotExpiry != nil && lotExpiry.ExpiryDate != nil {
		c.SetLotExpiryDate(*lotExpiry.ExpiryDate)
	}
	if input.PatientIDNumber != "" {
		c.SetPatientIDNumber(input.PatientIDNumber)
	}
	// NOTE: witness_staff_id is intentionally NOT re-read from input.WitnessStaffID here —
	// it was already set above from the verified step-up token. Trusting the request body
	// would let a client claim any witness without actually authorizing them.
	if input.Notes != "" {
		c.SetNotes(input.Notes)
	}

	logEntry, err := c.Save(r.Context())
	if err != nil {
		h.log.Error("create controlled substance log failed", zap.Error(err))
		jsonError(w, "failed to create log entry: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if h.auditSvc != nil {
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID:    tid,
			OutletID:    &outletID,
			ActorUserID: dispensedBy,
			ApproverID:  &approverUserID,
			Action:      "pharmacy.controlled_substance.dispense",
			EntityType:  "controlled_substance_log",
			EntityID:    logEntry.ID.String(),
			Reason:      "dual-person controlled substance dispense",
		})
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
