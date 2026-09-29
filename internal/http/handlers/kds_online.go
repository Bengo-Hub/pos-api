package handlers

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entkdsticket "github.com/bengobox/pos-service/internal/ent/kdsticket"
	entorderlink "github.com/bengobox/pos-service/internal/ent/orderlink"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	ordersmod "github.com/bengobox/pos-service/internal/modules/orders"
)

// kdsTicketView is a KDS ticket as the board renders it: the stored ticket plus where the order
// came from ("pos" or "online") and a short label the kitchen reads at a glance ("Online pickup",
// "Online delivery for Fri 18:30"). The ticket itself carries neither, so they are derived from the
// order's online link and metadata in one batched lookup per list request.
type kdsTicketView struct {
	*ent.KDSTicket
	OrderSource string `json:"order_source"`
	OrderLabel  string `json:"order_label,omitempty"`
	OrderNotes  string `json:"order_notes,omitempty"`
	// Job is set for services job orders (printing, garage, laundry): the production board shows
	// the customer, due date, current stage, brief, attachments and payment position from it.
	Job *kdsJobView `json:"job,omitempty"`
}

// kdsJobView is the job header the production board renders on a services ticket.
type kdsJobView struct {
	CustomerName  string         `json:"customer_name,omitempty"`
	CustomerPhone string         `json:"customer_phone,omitempty"`
	TotalAmount   float64        `json:"total_amount"`
	PaidTotal     float64        `json:"paid_total"`
	Details       map[string]any `json:"details,omitempty"`
}

// withOrderSource decorates tickets with their order source/label for the board.
func (h *KDSHandler) withOrderSource(ctx context.Context, tickets []*ent.KDSTicket) []kdsTicketView {
	out := make([]kdsTicketView, 0, len(tickets))
	if len(tickets) == 0 {
		return out
	}
	ids := make([]uuid.UUID, 0, len(tickets))
	seen := map[uuid.UUID]bool{}
	for _, t := range tickets {
		if !seen[t.OrderID] {
			seen[t.OrderID] = true
			ids = append(ids, t.OrderID)
		}
	}
	online := map[uuid.UUID]*ent.POSOrder{}
	if links, err := h.client.OrderLink.Query().Where(entorderlink.OrderIDIn(ids...)).All(ctx); err == nil && len(links) > 0 {
		linked := make([]uuid.UUID, 0, len(links))
		for _, l := range links {
			linked = append(linked, l.OrderID)
		}
		if orders, oerr := h.client.POSOrder.Query().Where(entposorder.IDIn(linked...)).All(ctx); oerr == nil {
			for _, o := range orders {
				online[o.ID] = o
			}
		}
	}
	// Services job headers: one batched query restricted to service_job orders, so a kitchen
	// board pays nothing extra.
	jobs := map[uuid.UUID]*kdsJobView{}
	if jobOrders, jerr := h.client.POSOrder.Query().
		Where(entposorder.IDIn(ids...), entposorder.OrderSubtypeEQ(entposorder.OrderSubtypeServiceJob)).
		All(ctx); jerr == nil {
		for _, o := range jobOrders {
			jv := &kdsJobView{TotalAmount: o.TotalAmount, PaidTotal: o.PaidTotal}
			if o.CustomerName != nil {
				jv.CustomerName = *o.CustomerName
			}
			if o.CustomerPhone != nil {
				jv.CustomerPhone = *o.CustomerPhone
			}
			if d, ok := o.Metadata["job"].(map[string]any); ok {
				jv.Details = d
			}
			jobs[o.ID] = jv
		}
	}
	for _, t := range tickets {
		v := kdsTicketView{KDSTicket: t, OrderSource: "pos", Job: jobs[t.OrderID]}
		if o, ok := online[t.OrderID]; ok {
			v.OrderSource = "online"
			v.OrderLabel = onlineOrderLabel(o)
			if n, _ := o.Metadata["order_notes"].(string); n != "" {
				v.OrderNotes = n
			}
		}
		out = append(out, v)
	}
	return out
}

// onlineOrderLabel is the one-line channel label shown on an online ticket.
func onlineOrderLabel(o *ent.POSOrder) string {
	label := "Online pickup"
	if ft, _ := o.Metadata["fulfillment_type"].(string); ft == "delivery" {
		label = "Online delivery"
	}
	if at, _ := o.Metadata["scheduled_for_label"].(string); at != "" {
		label = fmt.Sprintf("%s for %s", label, at)
	}
	return label
}

// externalOrderID returns the online (ordering-backend) order id linked to a POS order, or "".
func (h *KDSHandler) externalOrderID(ctx context.Context, orderID uuid.UUID) string {
	link, err := h.client.OrderLink.Query().Where(entorderlink.OrderID(orderID)).First(ctx)
	if err != nil {
		return ""
	}
	return link.ExternalOrderID
}

// publishOnlinePreparing tells ordering-backend the kitchen started an online order. Only the
// first start matters (ordering moves confirmed -> preparing once and ignores repeats), so nothing
// is published once another ticket of the order is already underway.
func (h *KDSHandler) publishOnlinePreparing(ctx context.Context, tid uuid.UUID, ticket *ent.KDSTicket) {
	if h.publisher == nil || ticket == nil {
		return
	}
	external := h.externalOrderID(ctx, ticket.OrderID)
	if external == "" {
		return
	}
	started, err := h.client.KDSTicket.Query().
		Where(
			entkdsticket.TenantID(tid),
			entkdsticket.OrderID(ticket.OrderID),
			entkdsticket.IDNEQ(ticket.ID),
			entkdsticket.StatusIn(entkdsticket.StatusInProgress, entkdsticket.StatusReady, entkdsticket.StatusServed),
		).
		Count(ctx)
	if err == nil && started > 0 {
		return
	}
	_ = h.publisher.PublishOnlineOrderPreparing(ctx, tid, map[string]any{
		"order_id":          ticket.OrderID,
		"order_number":      ticket.OrderNumber,
		"external_order_id": external,
	})
}

// settleOnlineOrderServed handles the moment the kitchen has served (passed out) every ticket of an
// ONLINE order. Such an order is prepaid or paid on collection/delivery through ordering, so it
// must not drop into the till's pending_payment queue or ping a waiter; it waits on the counter as
// ready for pickup (or for its rider). ordering is told the order is ready in case no ticket was
// ever bumped through "ready" first. Returns true when the order was an online order.
func (h *KDSHandler) settleOnlineOrderServed(ctx context.Context, tid, orderID uuid.UUID, orderNumber string) bool {
	external := h.externalOrderID(ctx, orderID)
	if external == "" {
		return false
	}
	if _, err := h.client.POSOrder.Update().
		Where(entposorder.ID(orderID), entposorder.TenantID(tid), entposorder.Status(ordersmod.StatusOpen)).
		SetStatus(onlineReadyStatus).
		Save(ctx); err != nil {
		h.log.Warn("kds: failed to mark online order ready", zap.Error(err), zap.String("order", orderNumber))
	}
	if h.publisher != nil {
		_ = h.publisher.PublishKDSOrderReady(ctx, tid, map[string]any{
			"order_id":          orderID,
			"order_number":      orderNumber,
			"external_order_id": external,
		})
	}
	return true
}

// onlineReadyStatus is the POS status an online order waits in once it is ready to hand over.
const onlineReadyStatus = "ready_for_pickup"
