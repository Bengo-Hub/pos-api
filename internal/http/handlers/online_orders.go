package handlers

import (
	"net/http"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entkdsticket "github.com/bengobox/pos-service/internal/ent/kdsticket"
	entorderlink "github.com/bengobox/pos-service/internal/ent/orderlink"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// OnlineOrderHandler handles click-and-collect / pickup order endpoints for the KDS,
// plus the WS-D delivery rider-assignment proxy/delegation endpoints.
type OnlineOrderHandler struct {
	log       *zap.Logger
	db        *ent.Client
	publisher *events.Publisher
	rider     *riderDeps // optional WS-D assign-rider dependencies (ordering client + logistics URL)
}

// NewOnlineOrderHandler creates a new OnlineOrderHandler.
func NewOnlineOrderHandler(log *zap.Logger, db *ent.Client) *OnlineOrderHandler {
	return &OnlineOrderHandler{log: log, db: db}
}

// SetPublisher wires the event publisher for online-order lifecycle events.
func (h *OnlineOrderHandler) SetPublisher(p *events.Publisher) { h.publisher = p }

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

// notCollectedFilter matches orders NOT yet marked collected (metadata.collected is absent/false).
func notCollectedFilter() predicate.POSOrder {
	return predicate.POSOrder(func(s *sql.Selector) {
		s.Where(sql.P(func(b *sql.Builder) {
			b.WriteString("(")
			b.WriteString(s.C("metadata"))
			b.WriteString("->>'collected' IS DISTINCT FROM 'true')")
		}))
	})
}

// ListPickup handles GET /{tenantID}/pos/online-orders/pickup
// Returns all active pickup / click-and-collect orders (not completed or cancelled).
// Supports optional ?status= and ?outlet_id= filters.
func (h *OnlineOrderHandler) ListPickup(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	filters := []predicate.POSOrder{
		posorder.TenantID(tid),
		// Online click-and-collect/pickup orders (metadata.source) PLUS POS-native TAKEAWAY orders
		// placed at the terminal — both are collected at the counter once the kitchen is done, so
		// they share the pickup queue.
		posorder.Or(
			pickupSourceFilter(),
			posorder.OrderSubtypeEQ(posorder.OrderSubtypeTakeaway),
		),
	}
	if status := r.URL.Query().Get("status"); status != "" {
		filters = append(filters, posorder.Status(status))
	} else {
		// Active queue = not cancelled/voided AND not yet collected. A paid (completed) order stays
		// here as "Ready for collection" until it's marked collected — then it moves to History.
		filters = append(filters, posorder.StatusNotIn("cancelled", "voided"), notCollectedFilter())
	}
	// Optional outlet scoping so a multi-outlet tenant's KDS only sees its own pickups.
	if outletParam := r.URL.Query().Get("outlet_id"); outletParam != "" {
		if outletID, perr := uuid.Parse(outletParam); perr == nil {
			filters = append(filters, posorder.OutletID(outletID))
		}
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

// ListPickupHistory handles GET /{tenantID}/pos/online-orders/history
// Returns the collection RECORDS for pickup / takeaway / delivery orders: those already collected
// (metadata.collected=true) and those that were never collected (cancelled/voided). Powers the
// History tab. Supports ?outcome=collected|uncollected and ?outlet_id=.
func (h *OnlineOrderHandler) ListPickupHistory(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	filters := []predicate.POSOrder{
		posorder.TenantID(tid),
		posorder.Or(
			pickupSourceFilter(),
			posorder.OrderSubtypeEQ(posorder.OrderSubtypeTakeaway),
			posorder.OrderSubtypeEQ(posorder.OrderSubtypeDelivery),
		),
	}
	switch r.URL.Query().Get("outcome") {
	case "uncollected":
		filters = append(filters, posorder.StatusIn("cancelled", "voided"))
	case "collected":
		filters = append(filters, collectedFilter())
	default:
		// Both: collected OR cancelled/voided.
		filters = append(filters, posorder.Or(collectedFilter(), posorder.StatusIn("cancelled", "voided")))
	}
	if outletParam := r.URL.Query().Get("outlet_id"); outletParam != "" {
		if outletID, perr := uuid.Parse(outletParam); perr == nil {
			filters = append(filters, posorder.OutletID(outletID))
		}
	}

	p := pagination.Parse(r)
	baseQ := h.db.POSOrder.Query().Where(filters...)
	total, _ := baseQ.Clone().Count(r.Context())
	orders, err := baseQ.Order(ent.Desc(posorder.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list pickup history failed", zap.Error(err))
		jsonError(w, "failed to list history", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(orders, total, p))
}

// collectedFilter matches orders marked collected (metadata.collected=true).
func collectedFilter() predicate.POSOrder {
	return predicate.POSOrder(func(s *sql.Selector) {
		s.Where(sql.P(func(b *sql.Builder) {
			b.WriteString("(")
			b.WriteString(s.C("metadata"))
			b.WriteString("->>'collected' = 'true')")
		}))
	})
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
// KDS marks the order as collected (completed) by the customer. This also serves any
// outstanding KDS tickets for the order (so they leave the kitchen display) and
// publishes pos.online_order.collected for ordering-backend to close the online order.
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

	// Stamp a collected flag in metadata (the pickup queue keeps paid-but-uncollected orders in a
	// "Ready for collection" state; collected ones move to History). Also mark the sale completed —
	// by the time it's collected it has been paid.
	meta := order.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	meta["collected"] = true
	meta["collected_at"] = time.Now().Format(time.RFC3339)
	updated, err := h.db.POSOrder.UpdateOneID(oid).
		SetStatus("completed").
		SetMetadata(meta).
		Save(r.Context())
	if err != nil {
		h.log.Error("mark order collected failed", zap.Error(err))
		jsonError(w, "failed to update order status", http.StatusInternalServerError)
		return
	}

	// Serve any still-active KDS tickets so they drop off the kitchen display.
	now := time.Now()
	if _, terr := h.db.KDSTicket.Update().
		Where(
			entkdsticket.TenantID(tid),
			entkdsticket.OrderID(oid),
			entkdsticket.StatusIn(
				entkdsticket.StatusPending,
				entkdsticket.StatusInProgress,
				entkdsticket.StatusReady,
			),
		).
		SetStatus(entkdsticket.StatusServed).
		SetCompletedAt(now).
		Save(r.Context()); terr != nil {
		h.log.Warn("mark-collected: failed to serve KDS tickets", zap.Error(terr), zap.Stringer("order_id", oid))
	}

	// Publish pos.online_order.collected so ordering-backend can close the online order.
	if h.publisher != nil {
		externalOrderID := ""
		if link, lerr := h.db.OrderLink.Query().
			Where(entorderlink.OrderID(oid)).
			First(r.Context()); lerr == nil {
			externalOrderID = link.ExternalOrderID
		}
		_ = h.publisher.PublishOnlineOrderCollected(r.Context(), tid, map[string]any{
			"external_order_id": externalOrderID,
			"order_number":      updated.OrderNumber,
			"tenant_id":         tid.String(),
			"source":            "click_and_collect",
		})
	}

	jsonOK(w, updated)
}
