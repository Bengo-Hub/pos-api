package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/cashdrawer"
	"github.com/bengobox/pos-service/internal/ent/posdevicesession"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// DrawerHandler handles cash drawer lifecycle endpoints.
type DrawerHandler struct {
	log       *zap.Logger
	client    *ent.Client
	publisher *events.Publisher
}

func NewDrawerHandler(log *zap.Logger, client *ent.Client, publisher *events.Publisher) *DrawerHandler {
	return &DrawerHandler{log: log, client: client, publisher: publisher}
}

type openDrawerInput struct {
	OutletID     string  `json:"outletId"`
	DeviceID     string  `json:"deviceId"`
	StartingCash float64 `json:"startingCash"`
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

	outletID := parseOptionalUUID(input.OutletID, r)
	deviceID, _ := uuid.Parse(input.DeviceID)

	now := time.Now()
	drawer, err := h.client.CashDrawer.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetDeviceID(deviceID).
		SetStartingCash(input.StartingCash).
		SetStatus("open").
		SetOpenedAt(now).
		Save(r.Context())
	if err != nil {
		h.log.Error("open drawer failed", zap.Error(err))
		jsonError(w, "failed to open drawer", http.StatusInternalServerError)
		return
	}

	// Auto-create a POSDeviceSession (shift) for this device if none is open.
	// This keeps the drawer and shift in sync without requiring a separate API call.
	if deviceID != uuid.Nil {
		openExists, _ := h.client.POSDeviceSession.Query().
			Where(posdevicesession.TenantID(tid), posdevicesession.DeviceID(deviceID), posdevicesession.SessionStatus("open")).
			Exist(r.Context())
		if !openExists {
			userID := uuid.Nil
			if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
				userID, _ = uuid.Parse(claims.Subject)
			}
			_, _ = h.client.POSDeviceSession.Create().
				SetTenantID(tid).
				SetDeviceID(deviceID).
				SetUserID(userID).
				SetSessionStatus("open").
				SetFloatAmount(input.StartingCash).
				SetOpenedAt(now).
				Save(r.Context())
		}
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

	if h.publisher != nil {
		go func() {
			data := map[string]any{
				"drawer_id":     updated.ID.String(),
				"tenant_id":     updated.TenantID.String(),
				"outlet_id":     updated.OutletID.String(),
				"starting_cash": updated.StartingCash,
				"ending_cash":   input.EndingCash,
				"variance":      input.EndingCash - updated.StartingCash,
				"opened_at":     updated.OpenedAt.Format(time.RFC3339),
				"closed_at":     now.Format(time.RFC3339),
			}
			if err := h.publisher.PublishDrawerClosed(r.Context(), updated.TenantID, data); err != nil {
				h.log.Warn("failed to publish pos.drawer.closed", zap.String("drawer_id", updated.ID.String()), zap.Error(err))
			}
		}()
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

	p := pagination.Parse(r)
	baseQ := h.client.CashDrawer.Query().Where(cashdrawer.TenantID(tid))
	total, _ := baseQ.Clone().Count(r.Context())
	drawers, err := baseQ.Order(ent.Desc(cashdrawer.FieldOpenedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, pagination.NewResponse(drawers, total, p))
}
