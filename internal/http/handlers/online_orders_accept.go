package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
)

// orderReleaser is the slice of the orders service the online-orders queue drives: opening an
// accepted, held order (KDS tickets + kitchen chits) and closing an order's open KDS tickets when it
// is handed over (served) or rejected (voided). Closing goes through the service so every live board
// on the outlet is told, the same as any other ticket change.
type orderReleaser interface {
	ReleaseHeldOrder(ctx context.Context, tenantID, orderID uuid.UUID) bool
	AutoClearKDSTicketsForOrder(ctx context.Context, tenantID, orderID uuid.UUID)
	VoidKDSTicketsForOrder(ctx context.Context, tenantID, orderID uuid.UUID)
}

// SetOrderReleaser wires the order service used when an order is accepted.
func (h *OnlineOrderHandler) SetOrderReleaser(r orderReleaser) { h.releaser = r }

// scheduledHoldWindow mirrors ordering's prep buffer: an accepted order scheduled further ahead
// than this stays off the kitchen board until ordering hands it over at its prep window.
const scheduledHoldWindow = 30 * time.Minute

// Accept handles POST /{tenantID}/pos/online-orders/{orderID}/accept.
// With manual order acceptance (the default) every online order waits in the queue until staff
// accept it. Acceptance is recorded in ordering-backend (which tells the customer the order is
// confirmed) and the held POS record is released to the kitchen at once, unless it is a scheduled
// order whose prep window has not opened yet.
func (h *OnlineOrderHandler) Accept(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	oid, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid orderID", http.StatusBadRequest)
		return
	}
	order, err := h.db.POSOrder.Get(r.Context(), oid)
	if err != nil || order.TenantID != tid {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}
	if order.Status != "awaiting_acceptance" {
		jsonOK(w, order) // already accepted (or no longer acceptable): nothing to do
		return
	}
	external := h.externalOrderID(r, oid)
	if external == "" {
		jsonError(w, "not an online order", http.StatusBadRequest)
		return
	}
	if h.rider == nil || h.rider.ordering == nil || !h.rider.ordering.Enabled() {
		jsonError(w, "ordering service not configured", http.StatusServiceUnavailable)
		return
	}
	slug := tenantSlugFromRequest(r)
	if slug == "" {
		jsonError(w, "tenant context required", http.StatusBadRequest)
		return
	}
	if err := h.rider.ordering.UpdateOrderStatus(r.Context(), slug, external, "confirmed"); err != nil {
		h.log.Warn("accept online order: ordering confirm failed", zap.Error(err), zap.String("external_order_id", external))
		jsonError(w, "could not accept the order: "+err.Error(), http.StatusBadGateway)
		return
	}

	meta := order.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	meta["accepted_at"] = time.Now().Format(time.RFC3339)
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		meta["accepted_by"] = claims.Subject
	}
	_ = h.db.POSOrder.UpdateOneID(oid).SetMetadata(meta).Exec(r.Context())

	if !scheduledForLater(meta) && h.releaser != nil {
		h.releaser.ReleaseHeldOrder(r.Context(), tid, oid)
	}
	updated, err := h.db.POSOrder.Get(r.Context(), oid)
	if err != nil {
		jsonOK(w, order)
		return
	}
	jsonOK(w, updated)
}

// scheduledForLater reports whether the order is promised for a time beyond its prep window.
func scheduledForLater(meta map[string]any) bool {
	raw, _ := meta["scheduled_for"].(string)
	if raw == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false
	}
	return at.After(time.Now().Add(scheduledHoldWindow))
}
