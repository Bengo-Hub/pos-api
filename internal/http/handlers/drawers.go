package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/cashdrawer"
)

// DrawerHandler handles cash drawer lifecycle endpoints.
type DrawerHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewDrawerHandler(log *zap.Logger, client *ent.Client) *DrawerHandler {
	return &DrawerHandler{log: log, client: client}
}

type openDrawerInput struct {
	OutletID     uuid.UUID `json:"outletId"`
	DeviceID     uuid.UUID `json:"deviceId"`
	StartingCash float64   `json:"startingCash"`
}

// OpenDrawer handles POST /{tenantID}/pos/drawers/open
func (h *DrawerHandler) OpenDrawer(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input openDrawerInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	now := time.Now()
	drawer, err := h.client.CashDrawer.Create().
		SetTenantID(tid).
		SetOutletID(input.OutletID).
		SetDeviceID(input.DeviceID).
		SetStartingCash(input.StartingCash).
		SetStatus("open").
		SetOpenedAt(now).
		Save(r.Context())
	if err != nil {
		h.log.Error("open drawer failed", zap.Error(err))
		jsonError(w, "failed to open drawer", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, drawer)
}

// GetCurrentDrawer handles GET /{tenantID}/pos/drawers/current
func (h *DrawerHandler) GetCurrentDrawer(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	drawer, err := h.client.CashDrawer.Query().
		Where(cashdrawer.TenantID(tid), cashdrawer.Status("open")).
		Order(ent.Desc(cashdrawer.FieldOpenedAt)).
		First(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonOK(w, map[string]any{"drawer": nil, "isOpen": false})
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"drawer": drawer, "isOpen": true})
}

type closeDrawerInput struct {
	EndingCash float64 `json:"endingCash"`
}

// CloseDrawer handles POST /{tenantID}/pos/drawers/{id}/close
func (h *DrawerHandler) CloseDrawer(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	drawerID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid drawer id", http.StatusBadRequest)
		return
	}

	var input closeDrawerInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	drawer, err := h.client.CashDrawer.Query().
		Where(cashdrawer.ID(drawerID), cashdrawer.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "drawer not found", http.StatusNotFound)
		return
	}

	now := time.Now()
	updated, err := drawer.Update().
		SetEndingCash(input.EndingCash).
		SetStatus("closed").
		SetClosedAt(now).
		Save(r.Context())
	if err != nil {
		jsonError(w, "close failed", http.StatusInternalServerError)
		return
	}

	jsonOK(w, updated)
}

// ListDrawerHistory handles GET /{tenantID}/pos/drawers
func (h *DrawerHandler) ListDrawerHistory(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	drawers, err := h.client.CashDrawer.Query().
		Where(cashdrawer.TenantID(tid)).
		Order(ent.Desc(cashdrawer.FieldOpenedAt)).
		Limit(50).
		All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"data": drawers, "total": len(drawers)})
}
