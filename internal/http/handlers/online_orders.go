package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"

	"github.com/bengobox/pos-service/internal/ent"
	entkdsticket "github.com/bengobox/pos-service/internal/ent/kdsticket"
	entorderlink "github.com/bengobox/pos-service/internal/ent/orderlink"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	entpospayment "github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	ordersmod "github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// OnlineOrderHandler handles click-and-collect / pickup order endpoints for the KDS,
// plus the WS-D delivery rider-assignment proxy/delegation endpoints.
type OnlineOrderHandler struct {
	log       *zap.Logger
	db        *ent.Client
	publisher *events.Publisher
	rider     *riderDeps // optional WS-D assign-rider dependencies (ordering client + logistics URL)
	releaser  orderReleaser
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
	// Outlet scoping so a multi-outlet tenant's counter only sees its own pickups: an explicit
	// ?outlet_id wins, otherwise the terminal's active outlet (X-Outlet-ID).
	if outletID, ok := requestOutletID(r); ok {
		filters = append(filters, posorder.OutletID(outletID))
	}

	p := pagination.Parse(r)
	baseQ := h.db.POSOrder.Query().Where(filters...)
	total, _ := baseQ.Clone().Count(r.Context())
	// Lines are loaded so the counter can check the bag against the order before handing it over.
	orders, err := baseQ.Clone().WithLines().Order(ent.Asc(posorder.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
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
	if outletID, ok := requestOutletID(r); ok {
		filters = append(filters, posorder.OutletID(outletID))
	}

	p := pagination.Parse(r)
	baseQ := h.db.POSOrder.Query().Where(filters...)
	total, _ := baseQ.Clone().Count(r.Context())
	orders, err := baseQ.Clone().WithLines().Order(ent.Desc(posorder.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list pickup history failed", zap.Error(err))
		jsonError(w, "failed to list history", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(orders, total, p))
}

// requestOutletID resolves the outlet a queue request is scoped to: ?outlet_id first, then the
// terminal's active outlet header.
func requestOutletID(r *http.Request) (uuid.UUID, bool) {
	for _, raw := range []string{r.URL.Query().Get("outlet_id"), r.Header.Get("X-Outlet-ID")} {
		if raw == "" {
			continue
		}
		if id, err := uuid.Parse(raw); err == nil {
			return id, true
		}
	}
	return uuid.Nil, false
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
// The counter (or a printer-only kitchen, or a retail shop that just packed the bag) marks the
// order ready for handover. For an online order this also tells ordering-backend, which notifies
// the customer ("ready for pickup") or, for delivery, releases it to the rider dispatch.
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
	switch order.Status {
	case "cancelled", "voided", "completed":
		jsonError(w, "order is already "+order.Status, http.StatusConflict)
		return
	case "awaiting_acceptance":
		jsonError(w, "accept the order first", http.StatusConflict)
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

	if external := h.externalOrderID(r, oid); external != "" && h.publisher != nil {
		_ = h.publisher.PublishKDSOrderReady(r.Context(), tid, map[string]any{
			"order_id":          oid,
			"order_number":      updated.OrderNumber,
			"external_order_id": external,
			"source":            "online_orders_queue",
		})
	}

	jsonOK(w, updated)
}

// markCollectedRequest is the optional body of the collected action.
type markCollectedRequest struct {
	// CashCollected confirms the counter took the amount due for an order that was not paid online
	// (cash / M-Pesa on collection). Required before a pay-on-collection online order is released.
	CashCollected bool `json:"cash_collected"`
	// PaymentMethod records how it was paid at the counter ("cash", "mpesa", "card").
	PaymentMethod string `json:"payment_method"`
	// Reference is the M-Pesa code when the customer paid the counter by M-Pesa.
	Reference string `json:"reference"`
	// CollectionCode is the 6-digit code the pickup customer shows at the counter.
	CollectionCode string `json:"collection_code"`
	// NoCodeReason lets the counter hand over without the code (phone dead, message deleted)
	// after checking the customer another way; it is recorded on the order.
	NoCodeReason string `json:"no_code_reason"`
}

// MarkCollected handles POST /{tenantID}/pos/online-orders/{orderID}/collected
// The order is handed over: the customer collected it at the counter, or (for an online delivery
// the outlet delivers with its own staff) it was delivered. Serves any outstanding KDS tickets and
// tells ordering-backend, which completes the online order, settles cash on collection/delivery
// and consumes the stock reservation.
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

	var body markCollectedRequest
	if r.ContentLength != 0 {
		_ = json.NewDecoder(r.Body).Decode(&body)
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
	switch order.Status {
	case "cancelled", "voided":
		jsonError(w, "order was "+order.Status, http.StatusConflict)
		return
	case "awaiting_acceptance":
		jsonError(w, "accept the order first", http.StatusConflict)
		return
	}

	meta := order.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	external := h.externalOrderID(r, oid)
	isOnline := external != ""
	isDelivery := string(order.OrderSubtype) == "delivery"

	// Pickup: hand the bag to the person holding the collection code. Without it the counter must
	// say how they checked the customer, which is kept on the order.
	if isOnline && !isDelivery && ordersmod.CollectionCodeRequired(meta) {
		reason := strings.TrimSpace(body.NoCodeReason)
		switch {
		case strings.TrimSpace(body.CollectionCode) != "":
			if !ordersmod.CollectionCodeMatches(meta, body.CollectionCode) {
				jsonError(w, "that code does not match this order", http.StatusUnprocessableEntity)
				return
			}
			meta["collection_code_verified"] = true
		case len(reason) >= 3:
			meta["collection_code_verified"] = false
			meta["collection_code_skipped_reason"] = reason
		default:
			jsonError(w, "ask the customer for their collection code", http.StatusConflict)
			return
		}
	}

	// Money already taken at the terminal (the order was rung through checkout) counts as
	// collected. Asking for it again here would have the counter record the same cash twice.
	if isOnline && !body.CashCollected && paidAtTerminal(order.TotalAmount, order.PaidTotal) {
		body.CashCollected = true
		body.PaymentMethod = firstNonEmptyStr(body.PaymentMethod, h.terminalPaymentMethod(r.Context(), oid))
		meta["paid_at_terminal"] = true
	}

	// A pay-on-collection online order cannot leave the counter until the money is taken.
	if isOnline {
		prepaid, _ := meta["prepaid"].(bool)
		if channel, _ := meta["payment_channel"].(string); channel == "mpesa_manual" && !prepaid {
			jsonError(w, "verify the customer's M-Pesa code before handing the order over", http.StatusConflict)
			return
		}
		if !prepaid && !body.CashCollected {
			due, _ := meta["amount_due"].(float64)
			jsonError(w, fmt.Sprintf("collect %.2f from the customer before handing the order over", due), http.StatusConflict)
			return
		}
		if !prepaid {
			meta["cash_collected"] = true
			meta["collected_payment_method"] = firstNonEmptyStr(body.PaymentMethod, "cash")
			if ref := strings.ToUpper(strings.TrimSpace(body.Reference)); ref != "" {
				meta["collected_reference"] = ref
			}
		}
	}

	// Stamp a collected flag in metadata (the pickup queue keeps paid-but-uncollected orders in a
	// "Ready for collection" state; collected ones move to History). Also mark the sale completed.
	meta["collected"] = true
	meta["collected_at"] = time.Now().Format(time.RFC3339)
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		meta["collected_by"] = claims.Subject
	}
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

	if isOnline && h.publisher != nil {
		payload := map[string]any{
			"external_order_id": external,
			"order_number":      updated.OrderNumber,
			"tenant_id":         tid.String(),
			"cash_collected":    body.CashCollected,
			"payment_method":    body.PaymentMethod,
			"reference":         strings.ToUpper(strings.TrimSpace(body.Reference)),
		}
		// The terminal payment already booked this money in treasury (as a POS intent), so
		// ordering must mark the order paid without settling its own intent a second time.
		if paid, _ := meta["paid_at_terminal"].(bool); paid {
			payload["paid_at_terminal"] = true
		}
		if isDelivery {
			// Delivered by the outlet's own staff (no rider app): ordering walks the order through
			// out-for-delivery to delivered, settling cash on delivery.
			payload["source"] = "own_delivery"
			_ = h.publisher.PublishOnlineOrderDelivered(r.Context(), tid, payload)
		} else {
			payload["source"] = "click_and_collect"
			_ = h.publisher.PublishOnlineOrderCollected(r.Context(), tid, payload)
		}
	}

	jsonOK(w, updated)
}

// paidAtTerminal reports whether terminal payments already cover the order (half-cent tolerance
// for float totals). A zero-total order is never "paid at terminal": there was nothing to take.
func paidAtTerminal(total, paid float64) bool {
	return total > 0 && paid+0.005 >= total
}

// terminalPaymentMethod is how the order's latest completed terminal payment was tendered
// ("cash", "mpesa", "card"), for telling ordering how a pay-on-collection order was settled.
func (h *OnlineOrderHandler) terminalPaymentMethod(ctx context.Context, orderID uuid.UUID) string {
	p, err := h.db.POSPayment.Query().
		Where(entpospayment.OrderID(orderID), entpospayment.Status("completed")).
		Order(ent.Desc(entpospayment.FieldOccurredAt)).
		First(ctx)
	if err != nil {
		return "cash"
	}
	if m, _ := p.PaymentData["method"].(string); m != "" {
		return m
	}
	return "cash"
}

// rejectRequest is the body of the reject action.
type rejectRequest struct {
	Reason string `json:"reason"`
}

// Reject handles POST /{tenantID}/pos/online-orders/{orderID}/reject
// The outlet cannot fulfil an online order (item out of stock, closing early, kitchen overloaded).
// ordering-backend owns the order, so the cancellation is delegated there: it releases the stock
// hold, refunds a prepaid order and notifies the customer. The POS record and its KDS tickets are
// voided here straight away so the kitchen stops immediately.
func (h *OnlineOrderHandler) Reject(w http.ResponseWriter, r *http.Request) {
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
	var body rejectRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	body.Reason = strings.TrimSpace(body.Reason)
	if body.Reason == "" {
		jsonError(w, "a reason is required so the customer knows why", http.StatusBadRequest)
		return
	}

	order, err := h.db.POSOrder.Get(r.Context(), oid)
	if err != nil || order.TenantID != tid {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}
	switch order.Status {
	case "completed", "cancelled", "voided":
		jsonError(w, "order is already "+order.Status, http.StatusConflict)
		return
	}
	external := h.externalOrderID(r, oid)
	if external == "" {
		jsonError(w, "not an online order; void it from the sales screen instead", http.StatusBadRequest)
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
	if err := h.rider.ordering.CancelOrder(r.Context(), slug, external, body.Reason); err != nil {
		h.log.Error("reject online order: ordering cancel failed", zap.Error(err), zap.String("external_order_id", external))
		jsonError(w, "could not cancel the online order; try again", http.StatusBadGateway)
		return
	}

	meta := order.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	meta["rejected_reason"] = body.Reason
	meta["rejected_at"] = time.Now().Format(time.RFC3339)
	updated, err := h.db.POSOrder.UpdateOneID(oid).SetStatus("cancelled").SetMetadata(meta).Save(r.Context())
	if err != nil {
		h.log.Warn("reject online order: local cancel failed (the ordering cancel event will retry it)", zap.Error(err))
		jsonOK(w, map[string]any{"status": "cancelled", "order_id": oid.String()})
		return
	}
	if _, terr := h.db.KDSTicket.Update().
		Where(
			entkdsticket.TenantID(tid),
			entkdsticket.OrderID(oid),
			entkdsticket.StatusIn(entkdsticket.StatusPending, entkdsticket.StatusInProgress, entkdsticket.StatusReady),
		).
		SetStatus(entkdsticket.StatusVoided).
		SetCompletedAt(time.Now()).
		Save(r.Context()); terr != nil {
		h.log.Warn("reject online order: failed to void KDS tickets", zap.Error(terr))
	}
	jsonOK(w, updated)
}

// externalOrderID returns the online (ordering-backend) order id linked to a POS order, or "".
func (h *OnlineOrderHandler) externalOrderID(r *http.Request, orderID uuid.UUID) string {
	link, err := h.db.OrderLink.Query().Where(entorderlink.OrderID(orderID)).First(r.Context())
	if err != nil {
		return ""
	}
	return link.ExternalOrderID
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
