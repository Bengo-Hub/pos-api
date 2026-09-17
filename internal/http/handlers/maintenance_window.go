package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	outletmw "github.com/bengobox/pos-service/internal/http/middleware"
)

// MaintenanceWindowHandler is the platform-owner-only tool for scheduling a tenant's maintenance
// window (product name: Repair Mode) — see internal/http/middleware/maintenance.go for the gate
// that actually enforces it on every other request. Deliberately scoped to ONE tenant per call:
// there is no fleet-wide "maintenance every tenant" route, matching the user's own requirement
// that this stays tenant-scoped.
type MaintenanceWindowHandler struct {
	client *ent.Client
	log    *zap.Logger
}

func NewMaintenanceWindowHandler(client *ent.Client, log *zap.Logger) *MaintenanceWindowHandler {
	return &MaintenanceWindowHandler{client: client, log: log.Named("maintenance-window")}
}

// RegisterRoutes mounts the tool. The caller MUST wrap this router group in requirePlatformOwner
// — this handler does not re-check itself, matching every other manual ops tool in this codebase.
func (h *MaintenanceWindowHandler) RegisterRoutes(r chi.Router) {
	r.Route("/maintenance-window/{tenant_id}", func(mw chi.Router) {
		mw.Get("/", h.Get)
		mw.Post("/", h.Schedule)
		mw.Delete("/", h.Cancel)
	})
}

type maintenanceWindowResponse struct {
	TenantID             string `json:"tenant_id"`
	TenantSlug           string `json:"tenant_slug"`
	StartsAt             string `json:"starts_at,omitempty"`
	EndsAt               string `json:"ends_at,omitempty"`
	Reason               string `json:"reason,omitempty"`
	ActivatedBy          string `json:"activated_by,omitempty"`
	CurrentlyUnderRepair bool   `json:"currently_under_repair"`
}

func toMaintenanceWindowResponse(t *ent.Tenant) maintenanceWindowResponse {
	out := maintenanceWindowResponse{
		TenantID:             t.ID.String(),
		TenantSlug:           t.Slug,
		CurrentlyUnderRepair: outletmw.UnderMaintenance(t, time.Now()),
	}
	if t.MaintenanceStartsAt != nil {
		out.StartsAt = t.MaintenanceStartsAt.UTC().Format(time.RFC3339)
	}
	if t.MaintenanceEndsAt != nil {
		out.EndsAt = t.MaintenanceEndsAt.UTC().Format(time.RFC3339)
	}
	if t.MaintenanceReason != nil {
		out.Reason = *t.MaintenanceReason
	}
	if t.MaintenanceActivatedBy != nil {
		out.ActivatedBy = *t.MaintenanceActivatedBy
	}
	return out
}

// Get returns the tenant's current (or most recently scheduled) maintenance window.
// GET /api/v1/maintenance-window/{tenant_id}
func (h *MaintenanceWindowHandler) Get(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant_id"))
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	t, err := h.client.Tenant.Get(r.Context(), tid)
	if err != nil {
		jsonError(w, "tenant not found", http.StatusNotFound)
		return
	}
	jsonOK(w, toMaintenanceWindowResponse(t))
}

type scheduleMaintenanceInput struct {
	StartsAt string `json:"starts_at"` // RFC3339, required
	EndsAt   string `json:"ends_at"`   // RFC3339, required, must be after starts_at
	Reason   string `json:"reason"`
}

// Schedule sets (or replaces) the tenant's maintenance window. Both bounds are required — a
// half-set window is never enforced (see middleware.UnderMaintenance), so this refuses to write
// one rather than silently no-op the lockout the caller asked for.
// POST /api/v1/maintenance-window/{tenant_id}  body: {starts_at, ends_at, reason}
func (h *MaintenanceWindowHandler) Schedule(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant_id"))
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input scheduleMaintenanceInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	startsAt, serr := time.Parse(time.RFC3339, input.StartsAt)
	if serr != nil {
		jsonError(w, "starts_at must be RFC3339", http.StatusBadRequest)
		return
	}
	endsAt, eerr := time.Parse(time.RFC3339, input.EndsAt)
	if eerr != nil {
		jsonError(w, "ends_at must be RFC3339", http.StatusBadRequest)
		return
	}
	if !endsAt.After(startsAt) {
		jsonError(w, "ends_at must be after starts_at", http.StatusBadRequest)
		return
	}

	activatedBy := "platform"
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims != nil && claims.Email != "" {
		activatedBy = claims.Email
	}

	upd := h.client.Tenant.UpdateOneID(tid).
		SetMaintenanceStartsAt(startsAt).
		SetMaintenanceEndsAt(endsAt).
		SetMaintenanceActivatedBy(activatedBy)
	if input.Reason != "" {
		upd = upd.SetMaintenanceReason(input.Reason)
	} else {
		upd = upd.ClearMaintenanceReason()
	}
	t, err := upd.Save(r.Context())
	if err != nil {
		jsonError(w, "tenant not found", http.StatusNotFound)
		return
	}
	outletmw.InvalidateMaintenanceCache(tid)

	h.log.Warn("maintenance window scheduled",
		zap.String("tenant_id", tid.String()), zap.String("tenant_slug", t.Slug),
		zap.String("starts_at", startsAt.UTC().Format(time.RFC3339)),
		zap.String("ends_at", endsAt.UTC().Format(time.RFC3339)),
		zap.String("activated_by", activatedBy))

	jsonOK(w, toMaintenanceWindowResponse(t))
}

// Cancel clears the tenant's maintenance window immediately, regardless of where "now" sits
// inside it — the tenant-scoped early-cancel path (vs just waiting for ends_at to elapse).
// DELETE /api/v1/maintenance-window/{tenant_id}
func (h *MaintenanceWindowHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant_id"))
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	t, err := h.client.Tenant.UpdateOneID(tid).
		ClearMaintenanceStartsAt().
		ClearMaintenanceEndsAt().
		Save(r.Context())
	if err != nil {
		jsonError(w, "tenant not found", http.StatusNotFound)
		return
	}
	outletmw.InvalidateMaintenanceCache(tid)
	h.log.Warn("maintenance window cancelled", zap.String("tenant_id", tid.String()), zap.String("tenant_slug", t.Slug))
	jsonOK(w, toMaintenanceWindowResponse(t))
}
