package handlers

import (
	"net/http"

	"entgo.io/ent/dialect/sql"
	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/predicate"
)

// OnlineOrderHandler handles click-and-collect / pickup order endpoints for the KDS.
type OnlineOrderHandler struct {
	log *zap.Logger
	db  *ent.Client
}

// NewOnlineOrderHandler creates a new OnlineOrderHandler.
func NewOnlineOrderHandler(log *zap.Logger, db *ent.Client) *OnlineOrderHandler {
	return &OnlineOrderHandler{log: log, db: db}
}

// pickupSourceFilter returns a predicate that matches orders whose metadata.source
// equals "click_and_collect" or "pickup" — the two values set by the pickup consumer.
func pickupSourceFilter() predicate.POSOrder {
	return predicate.POSOrder(func(s *sql.Selector) {
		s.Where(sql.P(func(b *sql.Builder) {
			b.WriteString("(")
			b.WriteString(s.C("metadata"))
			b.WriteString("->>'source' IN ('click_and_collect','pickup')")
			b.WriteString(")")
		}))
	})
}

// ListPickup handles GET /{tenantID}/pos/online-orders/pickup
// Returns all active pickup / click-and-collect orders (not completed or cancelled).
func (h *OnlineOrderHandler) ListPickup(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	filters := []predicate.POSOrder{
		posorder.TenantID(tid),
		pickupSourceFilter(),
	}
	if status := r.URL.Query().Get("status"); status != "" {
		filters = append(filters, posorder.Status(status))
	} else {
		filters = append(filters, posorder.StatusNotIn("completed", "cancelled"))
	}

	p := pagination.Parse(r)
	baseQ := h.db.POSOrder.Query().Where(filters...)
	total, _ := baseQ.Clone().Count(r.Context())
	orders, err := baseQ.Order(ent.Asc(posorder.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list pickup orders failed", zap.Error(err))
		jsonError(w, "failed to list pickup orders", http.StatusInternalServerError)
		return
	}

	jsonOK(w, pagination.NewResponse(orders, total, p))
}

// MarkReady handles POST /{tenantID}/pos/online-orders/{orderID}/ready
// KDS marks the order as ready for customer pickup.
func (h *OnlineOrderHandler) MarkReady(w http.ResponseWriter, r *http.Request) {
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
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		h.log.Error("get order for mark-ready failed", zap.Error(err))
		jsonError(w, "failed to get order", http.StatusInternalServerError)
		return
	}
	if order.TenantID != tid {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}

	updated, err := h.db.POSOrder.UpdateOneID(oid).
		SetStatus("ready_for_pickup").
		Save(r.Context())
	if err != nil {
		h.log.Error("mark order ready failed", zap.Error(err))
		jsonError(w, "failed to update order status", http.StatusInternalServerError)
		return
	}

	jsonOK(w, updated)
}

// MarkCollected handles POST /{tenantID}/pos/online-orders/{orderID}/collected
// KDS marks the order as collected (completed) by the customer.
func (h *OnlineOrderHandler) MarkCollected(w http.ResponseWriter, r *http.Request) {
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
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "order not found", http.StatusNotFound)
			return
		}
		h.log.Error("get order for mark-collected failed", zap.Error(err))
		jsonError(w, "failed to get order", http.StatusInternalServerError)
		return
	}
	if order.TenantID != tid {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}

	updated, err := h.db.POSOrder.UpdateOneID(oid).
		SetStatus("completed").
		Save(r.Context())
	if err != nil {
		h.log.Error("mark order collected failed", zap.Error(err))
		jsonError(w, "failed to update order status", http.StatusInternalServerError)
		return
	}

	jsonOK(w, updated)
}

