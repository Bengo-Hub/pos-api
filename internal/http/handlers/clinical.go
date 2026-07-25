package handlers

import (
	"net/http"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entuserrole "github.com/bengobox/pos-service/internal/ent/posuserroleassignment"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
	"github.com/bengobox/pos-service/internal/modules/documents"
	"github.com/bengobox/pos-service/internal/modules/inventory"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// ClinicalHandler implements the OPD clinical workflow: Records (patient registration) ->
// Triage (vitals) -> Examination (diagnosis, lab referral, prescribing) -> Lab (test results).
// Each stage is independently toggleable per outlet (OutletSetting.enable_*_module) so a small
// chemist can run none of it and a full clinic-attached pharmacy can run all of it — see
// moduleEnabled below, checked at the top of every mutating handler in the four clinical_*.go
// files. Reuses PharmacyHandler's already-built Prescription/reservation/checkout pipeline for
// the final pharmacy step rather than duplicating it.
type ClinicalHandler struct {
	log       *zap.Logger
	db        *ent.Client
	inventory *inventory.Client
	orderSvc  *orders.Service
	seq       *documents.SequenceService
	auditSvc  *audit.Service
	// pharmacy is reused for its already-built createPrescriptionCore (server numbering, DDI/
	// allergy pre-check, drug-line persistence) — the Examination stage's "prescribe" step must
	// behave identically to a standalone pharmacy-counter prescription, just with patient_id/
	// visit_id stamped on. Same package, so the unexported method is directly callable.
	pharmacy *PharmacyHandler
}

func NewClinicalHandler(log *zap.Logger, db *ent.Client, inventoryClient *inventory.Client, orderSvc *orders.Service, seq *documents.SequenceService) *ClinicalHandler {
	return &ClinicalHandler{log: log, db: db, inventory: inventoryClient, orderSvc: orderSvc, seq: seq}
}

func (h *ClinicalHandler) SetAuditService(svc *audit.Service) { h.auditSvc = svc }

// SetPharmacyHandler wires the pharmacy handler so Examination's "prescribe" step can reuse its
// prescription-creation core.
func (h *ClinicalHandler) SetPharmacyHandler(ph *PharmacyHandler) { h.pharmacy = ph }

// clinicalModule names the four toggleable OutletSetting.enable_*_module columns.
type clinicalModule string

const (
	moduleRecords     clinicalModule = "records"
	moduleTriage      clinicalModule = "triage"
	moduleExamination clinicalModule = "examination"
	moduleLab         clinicalModule = "lab"
)

// moduleEnabled checks the outlet's OutletSetting toggle for the given clinical stage. Outlets
// with no OutletSetting row yet (never configured) default to DISABLED — an unconfigured small
// pharmacy should never silently expose an OPD workflow it never opted into.
func (h *ClinicalHandler) moduleEnabled(r *http.Request, outletID uuid.UUID, mod clinicalModule) bool {
	s, err := h.db.OutletSetting.Query().Where(entoutletsetting.OutletID(outletID)).Only(r.Context())
	if err != nil {
		return false
	}
	switch mod {
	case moduleRecords:
		return s.EnableRecordsModule
	case moduleTriage:
		return s.EnableTriageModule
	case moduleExamination:
		return s.EnableExaminationModule
	case moduleLab:
		return s.EnableLabModule
	default:
		return false
	}
}

func (h *ClinicalHandler) requireModule(w http.ResponseWriter, r *http.Request, outletID uuid.UUID, mod clinicalModule) bool {
	if !h.moduleEnabled(r, outletID, mod) {
		jsonError(w, string(mod)+" module is not enabled for this outlet — turn it on in Settings", http.StatusForbidden)
		return false
	}
	return true
}

// prescriberDTO is one row in the prescriber picker (New Prescription form + Examination's
// prescribe step) — staff eligible to be recorded as a prescription's prescriber.
type prescriberDTO struct {
	StaffID       string `json:"staff_id"`
	UserID        string `json:"user_id"`
	Name          string `json:"name"`
	Role          string `json:"role"`
	LicenseNumber string `json:"license_number,omitempty"`
}

// prescriberEligibleRoles mirrors the RBAC roles that carry pos.pharmacy.approve (clinical
// sign-off) — StaffMember.Role is the fast path; POSUserRoleAssignment -> POSRoleV2.RoleCode
// covers staff on a custom/reassigned role.
var prescriberEligibleRoles = map[string]bool{"pharmacist": true, "doctor": true}

// ListPrescribers handles GET /{tenantID}/pos/pharmacy/prescribers?search=
// Returns active staff eligible to be a prescription's prescriber (pharmacist/doctor role, by
// StaffMember.Role or an assigned custom role of the same code), each with their license number
// so the create-prescription UI can auto-fill it on selection.
func (h *ClinicalHandler) ListPrescribers(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	staff, err := h.db.StaffMember.Query().
		Where(entstaff.TenantID(tid), entstaff.IsActive(true)).
		All(r.Context())
	if err != nil {
		jsonError(w, "failed to list prescribers", http.StatusInternalServerError)
		return
	}

	// Staff on a custom role need their POSRoleV2.role_code resolved via the assignment join —
	// batched in one query rather than N+1 per staff row.
	assignments, _ := h.db.POSUserRoleAssignment.Query().
		Where(entuserrole.TenantID(tid)).
		WithRole().
		All(r.Context())
	roleCodeByUser := make(map[uuid.UUID]string, len(assignments))
	for _, a := range assignments {
		if a.Edges.Role != nil {
			roleCodeByUser[a.UserID] = a.Edges.Role.RoleCode
		}
	}

	out := make([]prescriberDTO, 0, len(staff))
	for _, s := range staff {
		role := s.Role
		if rc, ok := roleCodeByUser[s.UserID]; ok && rc != "" {
			role = rc
		}
		if !prescriberEligibleRoles[role] {
			continue
		}
		dto := prescriberDTO{StaffID: s.ID.String(), UserID: s.UserID.String(), Name: s.Name, Role: role}
		if s.LicenseNumber != nil {
			dto.LicenseNumber = *s.LicenseNumber
		}
		out = append(out, dto)
	}
	jsonOK(w, map[string]any{"data": out})
}
