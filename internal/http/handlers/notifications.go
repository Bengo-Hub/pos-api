package handlers

import (
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entnotif "github.com/bengobox/pos-service/internal/ent/posnotification"
)

// NotificationsHandler handles in-app notification endpoints for POS staff.
type NotificationsHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewNotificationsHandler(log *zap.Logger, client *ent.Client) *NotificationsHandler {
	return &NotificationsHandler{log: log, client: client}
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

	q := h.client.PosNotification.Query().
		Where(
			entnotif.TenantID(tid),
			entnotif.UserID(userID),
		).
		Order(ent.Desc(entnotif.FieldCreatedAt)).
		Limit(50)

	if !includeRead {
		q = q.Where(entnotif.IsRead(false))
	}

	notifications, err := q.All(r.Context())
	if err != nil {
		h.log.Error("list notifications failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{
		"data":  notifications,
		"total": len(notifications),
	})
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
