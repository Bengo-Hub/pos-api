package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entwhd "github.com/bengobox/pos-service/internal/ent/webhookdelivery"
	entwh "github.com/bengobox/pos-service/internal/ent/webhooksubscription"
)

// WebhookHandler handles webhook subscription CRUD endpoints.
type WebhookHandler struct {
	log *zap.Logger
	db  *ent.Client
}

// NewWebhookHandler creates a new WebhookHandler.
func NewWebhookHandler(log *zap.Logger, db *ent.Client) *WebhookHandler {
	return &WebhookHandler{log: log, db: db}
}

// createWebhookInput is the body for POST /pos/webhooks.
type createWebhookInput struct {
	EventType string     `json:"event_type"`
	TargetURL string     `json:"target_url"`
	Secret    *string    `json:"secret"`
	OutletID  *uuid.UUID `json:"outlet_id"`
}

// updateWebhookInput is the body for PUT /pos/webhooks/{webhookID}.
type updateWebhookInput struct {
	EventType *string    `json:"event_type"`
	TargetURL *string    `json:"target_url"`
	Secret    *string    `json:"secret"`
	OutletID  *uuid.UUID `json:"outlet_id"`
	IsActive  *bool      `json:"is_active"`
}

// List handles GET /{tenantID}/pos/webhooks
func (h *WebhookHandler) List(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.WebhookSubscription.Query().
		Where(entwh.TenantID(tid))

	if et := r.URL.Query().Get("event_type"); et != "" {
		q = q.Where(entwh.EventType(et))
	}

	p := pagination.Parse(r)
	total, _ := q.Clone().Count(r.Context())
	subs, err := q.Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("webhooks list failed", zap.Error(err))
		jsonError(w, "failed to list webhooks", http.StatusInternalServerError)
		return
	}

	jsonOK(w, pagination.NewResponse(subs, total, p))
}

// Create handles POST /{tenantID}/pos/webhooks
func (h *WebhookHandler) Create(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var body createWebhookInput
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if body.EventType == "" || body.TargetURL == "" {
		jsonError(w, "event_type and target_url are required", http.StatusBadRequest)
		return
	}

	c := h.db.WebhookSubscription.Create().
		SetTenantID(tid).
		SetEventType(body.EventType).
		SetTargetURL(body.TargetURL).
		SetNillableSecret(body.Secret).
		SetNillableOutletID(body.OutletID)

	sub, err := c.Save(r.Context())
	if err != nil {
		h.log.Error("webhook create failed", zap.Error(err))
		jsonError(w, "failed to create webhook", http.StatusInternalServerError)
		return
	}

	jsonOK(w, sub)
}

// Update handles PUT /{tenantID}/pos/webhooks/{webhookID}
func (h *WebhookHandler) Update(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	wid, err := uuid.Parse(chi.URLParam(r, "webhookID"))
	if err != nil {
		jsonError(w, "invalid webhookID", http.StatusBadRequest)
		return
	}

	// Verify ownership
	sub, err := h.db.WebhookSubscription.Get(r.Context(), wid)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "webhook not found", http.StatusNotFound)
			return
		}
		h.log.Error("webhook get failed", zap.Error(err))
		jsonError(w, "failed to get webhook", http.StatusInternalServerError)
		return
	}
	if sub.TenantID != tid {
		jsonError(w, "webhook not found", http.StatusNotFound)
		return
	}

	var body updateWebhookInput
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	u := h.db.WebhookSubscription.UpdateOneID(wid).
		SetNillableEventType(body.EventType).
		SetNillableTargetURL(body.TargetURL).
		SetNillableSecret(body.Secret).
		SetNillableOutletID(body.OutletID).
		SetNillableIsActive(body.IsActive)

	updated, err := u.Save(r.Context())
	if err != nil {
		h.log.Error("webhook update failed", zap.Error(err))
		jsonError(w, "failed to update webhook", http.StatusInternalServerError)
		return
	}

	jsonOK(w, updated)
}

// Delete handles DELETE /{tenantID}/pos/webhooks/{webhookID}
func (h *WebhookHandler) Delete(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	wid, err := uuid.Parse(chi.URLParam(r, "webhookID"))
	if err != nil {
		jsonError(w, "invalid webhookID", http.StatusBadRequest)
		return
	}

	// Verify ownership
	sub, err := h.db.WebhookSubscription.Get(r.Context(), wid)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "webhook not found", http.StatusNotFound)
			return
		}
		h.log.Error("webhook get failed", zap.Error(err))
		jsonError(w, "failed to get webhook", http.StatusInternalServerError)
		return
	}
	if sub.TenantID != tid {
		jsonError(w, "webhook not found", http.StatusNotFound)
		return
	}

	if err := h.db.WebhookSubscription.DeleteOneID(wid).Exec(r.Context()); err != nil {
		h.log.Error("webhook delete failed", zap.Error(err))
		jsonError(w, "failed to delete webhook", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ListDeliveries handles GET /{tenantID}/pos/webhooks/{webhookID}/deliveries
func (h *WebhookHandler) ListDeliveries(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	wid, err := uuid.Parse(chi.URLParam(r, "webhookID"))
	if err != nil {
		jsonError(w, "invalid webhookID", http.StatusBadRequest)
		return
	}

	// Verify ownership
	sub, err := h.db.WebhookSubscription.Get(r.Context(), wid)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "webhook not found", http.StatusNotFound)
			return
		}
		h.log.Error("webhook get failed", zap.Error(err))
		jsonError(w, "failed to get webhook", http.StatusInternalServerError)
		return
	}
	if sub.TenantID != tid {
		jsonError(w, "webhook not found", http.StatusNotFound)
		return
	}

	p := pagination.Parse(r)
	baseQ := h.db.WebhookDelivery.Query().Where(entwhd.SubscriptionID(wid))
	total, _ := baseQ.Clone().Count(r.Context())
	deliveries, err := baseQ.Order(ent.Desc(entwhd.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("webhook deliveries list failed", zap.Error(err))
		jsonError(w, "failed to list deliveries", http.StatusInternalServerError)
		return
	}

	jsonOK(w, pagination.NewResponse(deliveries, total, p))
}
