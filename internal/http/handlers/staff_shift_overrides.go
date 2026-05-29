package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entov "github.com/bengobox/pos-service/internal/ent/staffshiftoverride"
	authclient "github.com/Bengo-Hub/shared-auth-client"
)

type StaffShiftOverrideHandler struct {
	log *zap.Logger
	db  *ent.Client
}

func NewStaffShiftOverrideHandler(log *zap.Logger, db *ent.Client) *StaffShiftOverrideHandler {
	return &StaffShiftOverrideHandler{log: log, db: db}
}

type createOverrideInput struct {
	Date         string `json:"date"`          // YYYY-MM-DD
	OverrideType string `json:"override_type"` // off_duty | manual_shift | half_day
	StartTime    string `json:"start_time"`    // HH:MM (required for manual_shift/half_day)
	EndTime      string `json:"end_time"`      // HH:MM (required for manual_shift/half_day)
	Reason       string `json:"reason"`
}

// ListStaffOverrides handles GET /{tenantID}/pos/staff/{staffID}/overrides
func (h *StaffShiftOverrideHandler) ListStaffOverrides(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	staffID, err := uuid.Parse(chi.URLParam(r, "staffID"))
	if err != nil {
		jsonError(w, "invalid staff_id", http.StatusBadRequest)
		return
	}

	q := r.URL.Query()
	query := h.db.StaffShiftOverride.Query().
		Where(entov.TenantID(tid), entov.StaffMemberID(staffID)).
		Order(ent.Asc(entov.FieldDate))

	if from := q.Get("from"); from != "" {
		if t, err := time.Parse("2006-01-02", from); err == nil {
			query = query.Where(entov.DateGTE(t))
		}
	}
	if to := q.Get("to"); to != "" {
		if t, err := time.Parse("2006-01-02", to); err == nil {
			query = query.Where(entov.DateLTE(t))
		}
	}

	overrides, err := query.All(r.Context())
	if err != nil {
		h.log.Error("list staff overrides failed", zap.Error(err))
		jsonError(w, "failed to list overrides", http.StatusInternalServerError)
		return
	}
	jsonOK(w, overrides)
}

// ListAllOverrides handles GET /{tenantID}/pos/staff/overrides (all staff for a date range)
func (h *StaffShiftOverrideHandler) ListAllOverrides(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := r.URL.Query()
	query := h.db.StaffShiftOverride.Query().
		Where(entov.TenantID(tid)).
		Order(ent.Asc(entov.FieldDate))

	if from := q.Get("from"); from != "" {
		if t, err := time.Parse("2006-01-02", from); err == nil {
			query = query.Where(entov.DateGTE(t))
		}
	}
	if to := q.Get("to"); to != "" {
		if t, err := time.Parse("2006-01-02", to); err == nil {
			query = query.Where(entov.DateLTE(t))
		}
	}

	overrides, err := query.All(r.Context())
	if err != nil {
		h.log.Error("list all overrides failed", zap.Error(err))
		jsonError(w, "failed to list overrides", http.StatusInternalServerError)
		return
	}
	jsonOK(w, overrides)
}

// CreateOverride handles POST /{tenantID}/pos/staff/{staffID}/overrides
func (h *StaffShiftOverrideHandler) CreateOverride(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	staffID, err := uuid.Parse(chi.URLParam(r, "staffID"))
	if err != nil {
		jsonError(w, "invalid staff_id", http.StatusBadRequest)
		return
	}

	var inp createOverrideInput
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	dateVal, err := time.Parse("2006-01-02", inp.Date)
	if err != nil {
		jsonError(w, "invalid date — use YYYY-MM-DD", http.StatusBadRequest)
		return
	}

	if entov.OverrideTypeValidator(entov.OverrideType(inp.OverrideType)) != nil {
		jsonError(w, "override_type must be off_duty, manual_shift, or half_day", http.StatusBadRequest)
		return
	}

	claims, _ := authclient.ClaimsFromContext(r.Context())
	creatorID, _ := uuid.Parse(claims.Subject)

	creator := h.db.StaffShiftOverride.Create().
		SetTenantID(tid).
		SetStaffMemberID(staffID).
		SetDate(dateVal).
		SetOverrideType(entov.OverrideType(inp.OverrideType)).
		SetCreatedBy(creatorID).
		SetStatus(entov.StatusApproved) // manager-created overrides are auto-approved

	if inp.StartTime != "" {
		creator = creator.SetStartTime(inp.StartTime)
	}
	if inp.EndTime != "" {
		creator = creator.SetEndTime(inp.EndTime)
	}
	if inp.Reason != "" {
		creator = creator.SetReason(inp.Reason)
	}

	saved, err := creator.Save(r.Context())
	if err != nil {
		h.log.Error("create override failed", zap.Error(err))
		jsonError(w, "failed to create override", http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusCreated, saved)
}

// DeleteOverride handles DELETE /{tenantID}/pos/staff/{staffID}/overrides/{overrideID}
func (h *StaffShiftOverrideHandler) DeleteOverride(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	overrideID, err := uuid.Parse(chi.URLParam(r, "overrideID"))
	if err != nil {
		jsonError(w, "invalid override_id", http.StatusBadRequest)
		return
	}

	existing, err := h.db.StaffShiftOverride.Get(r.Context(), overrideID)
	if err != nil || existing.TenantID != tid {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}

	if err := h.db.StaffShiftOverride.DeleteOneID(overrideID).Exec(r.Context()); err != nil {
		h.log.Error("delete override failed", zap.Error(err))
		jsonError(w, "failed to delete override", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
