package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posdevicesession"
)

// DeviceHandler handles device session (shift) endpoints.
type DeviceHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewDeviceHandler(log *zap.Logger, client *ent.Client) *DeviceHandler {
	return &DeviceHandler{log: log, client: client}
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

	// Use provided device_id or a deterministic UUID derived from tenant+user for "current" device
	deviceID := uuid.New()
	if input.DeviceID != nil {
		deviceID = *input.DeviceID
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
