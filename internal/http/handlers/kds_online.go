package handlers

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entkdsticket "github.com/bengobox/pos-service/internal/ent/kdsticket"
	entorderlink "github.com/bengobox/pos-service/internal/ent/orderlink"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/modules/orderchannel"
	ordersmod "github.com/bengobox/pos-service/internal/modules/orders"
)

// kdsTicketView is a KDS ticket as the board renders it: the stored ticket plus where the order
// came from ("pos" or "online"), its channel (dine_in, takeaway, delivery, room_service, bar_tab,
// online_pickup, online_delivery, service_job) and a short label the kitchen reads at a glance
// ("Table 5", "Online delivery for Fri 18:30"). The ticket carries none of these, so they are derived
// from the order (orderchannel) in one batched lookup per list request. The board groups and filters
// on channel only, so every count it shows comes from the same classification.
type kdsTicketView struct {
	*ent.KDSTicket
	OrderSource  string `json:"order_source"`
	Channel      string `json:"channel"`
	OrderLabel   string `json:"order_label,omitempty"`
	OrderNotes   string `json:"order_notes,omitempty"`
	CustomerName string `json:"customer_name,omitempty"`
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
	// One query for every order on the board, only the columns the view needs.
	orders := map[uuid.UUID]*ent.POSOrder{}
	if rows, err := h.client.POSOrder.Query().
		Where(entposorder.IDIn(ids...)).
		Select(
			entposorder.FieldID, entposorder.FieldOrderSubtype, entposorder.FieldMetadata,
			entposorder.FieldCustomerName, entposorder.FieldCustomerPhone,
			entposorder.FieldTotalAmount, entposorder.FieldPaidTotal,
		).
		All(ctx); err == nil {
		for _, o := range rows {
			orders[o.ID] = o
		}
	} else {
		h.log.Warn("kds: order lookup for ticket view failed", zap.Error(err))
	}
	for _, t := range tickets {
		out = append(out, ticketView(t, orders[t.OrderID]))
	}
	return out
}

// ticketView decorates one ticket from its order. A ticket whose order is gone still renders, from
// the subtype stored on the ticket itself.
func ticketView(t *ent.KDSTicket, o *ent.POSOrder) kdsTicketView {
	subtype := t.OrderSubtype
	var meta map[string]any
	if o != nil {
		subtype = string(o.OrderSubtype)
		meta = o.Metadata
	}
	channel := orderchannel.Of(subtype, meta)
	v := kdsTicketView{
		KDSTicket:   t,
		OrderSource: orderchannel.Source(meta),
		Channel:     string(channel),
		OrderLabel:  ticketLabel(channel, t.TableReference, subtype, meta),
	}
	if o == nil {
		return v
	}
	if n, _ := meta["order_notes"].(string); n != "" {
		v.OrderNotes = n
	}
	// The counter calls takeaway and online orders by name.
	if channel.IsCounterHandover() && o.CustomerName != nil {
		v.CustomerName = *o.CustomerName
	}
	// Services job orders: the production board shows customer, stage, due date and payment.
	if channel == orderchannel.ServiceJob {
		jv := &kdsJobView{TotalAmount: o.TotalAmount, PaidTotal: o.PaidTotal}
		if o.CustomerName != nil {
			jv.CustomerName = *o.CustomerName
		}
		if o.CustomerPhone != nil {
			jv.CustomerPhone = *o.CustomerPhone
		}
		if d, ok := meta["job"].(map[string]any); ok {
			jv.Details = d
		}
		v.Job = jv
	}
	return v
}

// ticketLabel is the one-line route label on a ticket: the online channel and promised time for
// an online order, otherwise the table or room it goes to.
func ticketLabel(channel orderchannel.Channel, tableRef, subtype string, meta map[string]any) string {
	if label := orderchannel.OnlineLabel(subtype, meta); label != "" {
		return label
	}
	ref := strings.TrimSpace(tableRef)
	if ref == "" {
		return ""
	}
	switch channel {
	case orderchannel.DineIn, orderchannel.BarTab:
		if strings.HasPrefix(strings.ToLower(ref), "table") {
			return ref
		}
		return "Table " + ref
	case orderchannel.RoomService:
		if strings.HasPrefix(strings.ToLower(ref), "room") {
			return ref
		}
		return "Room " + ref
	}
	return ref
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
