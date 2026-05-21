package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posdevice"
	"github.com/bengobox/pos-service/internal/ent/posdevicesession"
	"github.com/bengobox/pos-service/internal/ent/posorder"
)

// DeviceHandler handles device session (shift) endpoints.
type DeviceHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewDeviceHandler(log *zap.Logger, client *ent.Client) *DeviceHandler {
	return &DeviceHandler{log: log, client: client}
}

// ListDevices handles GET /{tenantID}/pos/devices
// Returns all registered terminals for the tenant with outlet info.
func (h *DeviceHandler) ListDevices(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	devices, err := h.client.POSDevice.Query().
		Where(posdevice.TenantID(tid)).
		WithOutlet().
		Order(ent.Desc(posdevice.FieldRegisteredAt)).
		All(r.Context())
	if err != nil {
		h.log.Error("list devices failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	type deviceResp struct {
		ID           string  `json:"id"`
		DeviceCode   string  `json:"device_code"`
		DeviceType   string  `json:"device_type"`
		Status       string  `json:"status"`
		OutletName   string  `json:"outlet_name,omitempty"`
		LastSeenAt   *string `json:"last_seen_at,omitempty"`
		RegisteredAt string  `json:"registered_at"`
	}

	result := make([]deviceResp, 0, len(devices))
	for _, d := range devices {
		dr := deviceResp{
			ID:           d.ID.String(),
			DeviceCode:   d.DeviceCode,
			DeviceType:   d.DeviceType,
			Status:       d.Status,
			RegisteredAt: d.RegisteredAt.Format(time.RFC3339),
		}
		if d.LastSeenAt != nil {
			s := d.LastSeenAt.Format(time.RFC3339)
			dr.LastSeenAt = &s
		}
		if d.Edges.Outlet != nil {
			dr.OutletName = d.Edges.Outlet.Name
		}
		result = append(result, dr)
	}

	jsonOK(w, map[string]any{"data": result, "total": len(result)})
}

// GetCurrentSession handles GET /{tenantID}/pos/devices/current/sessions/current
// Returns the open session for the currently authenticated user, or 404.
func (h *DeviceHandler) GetCurrentSession(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	userID := currentUserID(r)

	session, err := h.client.POSDeviceSession.Query().
		Where(
			posdevicesession.TenantID(tid),
			posdevicesession.UserID(userID),
			posdevicesession.SessionStatus("open"),
		).
		Order(ent.Desc(posdevicesession.FieldOpenedAt)).
		First(r.Context())
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		jsonOK(w, nil)
		return
	}

	jsonOK(w, session)
}

type openSessionInput struct {
	OpeningFloat float64        `json:"opening_float"`
	DeviceID     *uuid.UUID     `json:"device_id,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// OpenSession handles POST /{tenantID}/pos/devices/current/sessions/open
func (h *DeviceHandler) OpenSession(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	userID := currentUserID(r)

	var input openSessionInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Resolve the device_id — must be a real POSDevice FK or session creation fails.
	deviceID, err := h.resolveOrCreateDevice(r, tid, input.DeviceID)
	if err != nil {
		h.log.Error("could not resolve device", zap.Error(err))
		jsonError(w, "failed to resolve device", http.StatusInternalServerError)
		return
	}

	meta := input.Metadata
	if meta == nil {
		meta = map[string]any{}
	}

	session, err := h.client.POSDeviceSession.Create().
		SetTenantID(tid).
		SetDeviceID(deviceID).
		SetUserID(userID).
		SetSessionStatus("open").
		SetFloatAmount(input.OpeningFloat).
		SetOpenedAt(time.Now()).
		SetMetadata(meta).
		Save(r.Context())
	if err != nil {
		h.log.Error("open session failed", zap.Error(err))
		jsonError(w, "failed to open session", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, session)
}

// resolveOrCreateDevice finds an existing POSDevice for the outlet (from JWT claims or
// input.DeviceID), or creates a web_terminal device on first use.
// The POSDeviceSession schema requires a valid device_id FK.
func (h *DeviceHandler) resolveOrCreateDevice(r *http.Request, tid uuid.UUID, inputDeviceID *uuid.UUID) (uuid.UUID, error) {
	ctx := r.Context()

	// Caller-supplied device_id takes precedence when it's a real FK.
	if inputDeviceID != nil {
		exists, _ := h.client.POSDevice.Query().
			Where(posdevice.ID(*inputDeviceID), posdevice.TenantID(tid)).
			Exist(ctx)
		if exists {
			return *inputDeviceID, nil
		}
	}

	// Resolve outlet from JWT claims (terminal sessions have OutletID in claims).
	var outletID uuid.UUID
	if claims, ok := authclient.ClaimsFromContext(ctx); ok && claims.OutletID != "" {
		if id, err := uuid.Parse(claims.OutletID); err == nil {
			outletID = id
		}
	}
	if outletID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("outlet_id not available in claims")
	}

	// Look for an existing web_terminal device for this outlet.
	device, err := h.client.POSDevice.Query().
		Where(posdevice.TenantID(tid), posdevice.OutletID(outletID), posdevice.DeviceType("web_terminal")).
		First(ctx)
	if err == nil {
		return device.ID, nil
	}
	if !ent.IsNotFound(err) {
		return uuid.Nil, err
	}

	// Create a web_terminal device for this outlet on first use.
	device, err = h.client.POSDevice.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetDeviceCode(fmt.Sprintf("WEB-%s", outletID.String()[:8])).
		SetDeviceType("web_terminal").
		SetStatus("active").
		Save(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	return device.ID, nil
}

type closeSessionInput struct {
	ClosingFloat float64        `json:"closing_float"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// CloseSession handles POST /{tenantID}/pos/devices/current/sessions/close
func (h *DeviceHandler) CloseSession(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	userID := currentUserID(r)

	var input closeSessionInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	session, err := h.client.POSDeviceSession.Query().
		Where(
			posdevicesession.TenantID(tid),
			posdevicesession.UserID(userID),
			posdevicesession.SessionStatus("open"),
		).
		Order(ent.Desc(posdevicesession.FieldOpenedAt)).
		First(r.Context())
	if err != nil {
		jsonError(w, "no open session found", http.StatusNotFound)
		return
	}

	now := time.Now()
	updated, err := h.client.POSDeviceSession.UpdateOne(session).
		SetSessionStatus("closed").
		SetClosedAt(now).
		Save(r.Context())
	if err != nil {
		h.log.Error("close session failed", zap.Error(err))
		jsonError(w, "failed to close session", http.StatusInternalServerError)
		return
	}

	jsonOK(w, updated)
}

// GetSessionSummary handles GET /{tenantID}/pos/devices/current/sessions/current/summary
// Returns aggregated sales stats for the active shift: order count and revenue.
func (h *DeviceHandler) GetSessionSummary(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	userID := currentUserID(r)

	session, err := h.client.POSDeviceSession.Query().
		Where(
			posdevicesession.TenantID(tid),
			posdevicesession.UserID(userID),
			posdevicesession.SessionStatus("open"),
		).
		Order(ent.Desc(posdevicesession.FieldOpenedAt)).
		First(r.Context())
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		jsonOK(w, nil)
		return
	}

	// Query completed orders created on this device since the session opened.
	orders, err := h.client.POSOrder.Query().
		Where(
			posorder.TenantID(tid),
			posorder.DeviceID(session.DeviceID),
			posorder.StatusEQ("completed"),
			posorder.CreatedAtGTE(session.OpenedAt),
		).
		All(r.Context())
	if err != nil {
		h.log.Error("session summary: order query failed", zap.Error(err))
		jsonError(w, "failed to compute session summary", http.StatusInternalServerError)
		return
	}

	var totalRevenue float64
	for _, o := range orders {
		totalRevenue += o.TotalAmount
	}

	jsonOK(w, map[string]any{
		"session_id":     session.ID,
		"opened_at":      session.OpenedAt,
		"opening_float":  session.FloatAmount,
		"order_count":    len(orders),
		"total_revenue":  totalRevenue,
		"expected_cash":  session.FloatAmount + totalRevenue,
	})
}

// currentUserID extracts the user UUID from JWT claims; falls back to nil UUID.
func currentUserID(r *http.Request) uuid.UUID {
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		return uuid.Nil
	}
	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil
	}
	return id
}
