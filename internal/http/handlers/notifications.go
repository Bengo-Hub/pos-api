package handlers

import (
	"net/http"

	"github.com/Bengo-Hub/pagination"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"nhooyr.io/websocket"

	"github.com/bengobox/pos-service/internal/ent"
	entnotif "github.com/bengobox/pos-service/internal/ent/posnotification"
	notifmod "github.com/bengobox/pos-service/internal/modules/notifications"
)

// NotificationsHandler handles in-app notification endpoints for POS staff.
type NotificationsHandler struct {
	log      *zap.Logger
	client   *ent.Client
	notifHub *notifmod.Hub
}

func NewNotificationsHandler(log *zap.Logger, client *ent.Client) *NotificationsHandler {
	return &NotificationsHandler{
		log:      log,
		client:   client,
		notifHub: notifmod.NewHub(log),
	}
}

// Hub returns the notification hub so KDSHandler can broadcast to it.
func (h *NotificationsHandler) Hub() *notifmod.Hub {
	return h.notifHub
}

// StreamNotifications handles GET /{tenantID}/pos/notifications/stream
// POS terminals connect here on login to receive real-time push alerts
// (order ready for payment, waiter called, etc.) with sound triggers.
func (h *NotificationsHandler) StreamNotifications(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	userID, ok := callerUserID(r)
	if !ok {
		// Fallback: allow ?user_id= for terminal JWTs that may not have Subject
		if uidStr := r.URL.Query().Get("user_id"); uidStr != "" {
			userID, err = uuid.Parse(uidStr)
			if err != nil {
				jsonError(w, "invalid user_id", http.StatusBadRequest)
				return
			}
		} else {
			jsonError(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
	}

	conn, wsErr := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: false,
		OriginPatterns:     []string{"*"},
	})
	if wsErr != nil {
		h.log.Warn("notifications: websocket upgrade failed", zap.Error(wsErr))
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	h.log.Info("notifications: client connected",
		zap.Stringer("tenant_id", tid),
		zap.Stringer("user_id", userID),
	)

	h.notifHub.ServeWS(r.Context(), conn, tid, userID)

	h.log.Debug("notifications: client disconnected",
		zap.Stringer("tenant_id", tid),
		zap.Stringer("user_id", userID),
	)
}

func callerUserID(r *http.Request) (uuid.UUID, bool) {
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		return uuid.Nil, false
	}
	uid, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil, false
	}
	return uid, true
}

// List handles GET /{tenantID}/pos/notifications
// Returns recent notifications for the calling user. Pass ?include_read=true to include read ones.
func (h *NotificationsHandler) List(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	userID, ok := callerUserID(r)
	if !ok {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	includeRead := r.URL.Query().Get("include_read") == "true"

	baseQ := h.client.PosNotification.Query().
		Where(
			entnotif.TenantID(tid),
			entnotif.UserID(userID),
		)

	if !includeRead {
		baseQ = baseQ.Where(entnotif.IsRead(false))
	}

	p := pagination.Parse(r)
	total, _ := baseQ.Clone().Count(r.Context())
	notifications, err := baseQ.Order(ent.Desc(entnotif.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list notifications failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, pagination.NewResponse(notifications, total, p))
}

// MarkRead handles PATCH /{tenantID}/pos/notifications/{id}/read
func (h *NotificationsHandler) MarkRead(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	notifID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid notification id", http.StatusBadRequest)
		return
	}

	userID, _ := callerUserID(r)

	n, err := h.client.PosNotification.Query().
		Where(entnotif.ID(notifID), entnotif.TenantID(tid), entnotif.UserID(userID)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "notification not found", http.StatusNotFound)
		return
	}

	if _, err := n.Update().SetIsRead(true).Save(r.Context()); err != nil {
		h.log.Error("mark notification read failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"ok": true})
}

// MarkAllRead handles POST /{tenantID}/pos/notifications/mark-all-read
func (h *NotificationsHandler) MarkAllRead(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	userID, ok := callerUserID(r)
	if !ok {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	count, err := h.client.PosNotification.Update().
		Where(entnotif.TenantID(tid), entnotif.UserID(userID), entnotif.IsRead(false)).
		SetIsRead(true).
		Save(r.Context())
	if err != nil {
		h.log.Error("mark all notifications read failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"marked_read": count})
}
