package orders

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/kdsticket"
	"github.com/bengobox/pos-service/internal/ent/orderlink"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	kdsmod "github.com/bengobox/pos-service/internal/modules/kds"
)

// Online-order lifecycle mirroring. ordering-backend owns an online order; the POS record is the
// outlet's working copy. These handlers keep that copy in step with what happens outside the
// outlet so staff never act on a stale card:
//   - cancelled (customer, admin, payment failure, or a reject from this POS): the POS order and
//     every open KDS ticket are voided so the kitchen stops;
//   - out_for_delivery: the rider collected it; the card shows it left the counter;
//   - delivered / completed (rider proof of delivery, ordering staff dashboard): the order is
//     closed and moves to the queue history.

// SubscribeToOnlineCancellations consumes ordering.order.cancelled (CancelOrder publishes this
// subject rather than a status.changed event).
func (s *KDSOrderingSubscriber) SubscribeToOnlineCancellations(nc *nats.Conn) error {
	if nc == nil {
		return fmt.Errorf("online cancellation subscriber: NATS connection is nil")
	}
	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("online cancellation subscriber: jetstream init: %w", err)
	}
	sharedevents.SubscribeQueueWithRebind(s.logger, js, "ordering", "ordering.order.cancelled", "pos-online-order-cancelled", func(msg *nats.Msg) {
		var evt orderingStatusChangedEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			_ = msg.Ack()
			return
		}
		if err := s.applyOnlineLifecycle(context.Background(), &evt, "cancelled"); err != nil {
			s.logger.Error("online cancellation: handler failed", zap.Error(err))
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	},
		nats.BindStream("ordering"),
		nats.Durable("pos-online-order-cancelled"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(5),
	)
	s.logger.Info("online cancellation subscriber started", zap.String("subject", "ordering.order.cancelled"))
	return nil
}

// isLifecycleStatus reports whether an ordering status is mirrored onto the POS record.
func isLifecycleStatus(status string) bool {
	switch status {
	case "cancelled", "out_for_delivery", "delivered", "completed", "refunded":
		return true
	}
	return false
}

// applyOnlineLifecycle mirrors one ordering lifecycle status onto the linked POS order.
func (s *KDSOrderingSubscriber) applyOnlineLifecycle(ctx context.Context, evt *orderingStatusChangedEvent, status string) error {
	orderIDStr, _ := evt.Data["order_id"].(string)
	if orderIDStr == "" {
		return nil
	}
	links, err := s.client.OrderLink.Query().
		Where(orderlink.ExternalOrderID(orderIDStr), orderlink.ChannelSourceIn(channelClickAndCollect, channelDelivery)).
		All(ctx)
	if err != nil {
		return fmt.Errorf("query order link: %w", err)
	}
	if len(links) == 0 {
		return nil // not ingested into POS (ordering has other channels too)
	}
	for _, link := range links {
		order, gerr := s.client.POSOrder.Get(ctx, link.OrderID)
		if gerr != nil {
			if ent.IsNotFound(gerr) {
				continue
			}
			return gerr
		}
		if err := s.mirrorStatus(ctx, order, status, evt); err != nil {
			return err
		}
	}
	return nil
}

// mirrorStatus applies a lifecycle status to one POS order (idempotent).
func (s *KDSOrderingSubscriber) mirrorStatus(ctx context.Context, order *ent.POSOrder, status string, evt *orderingStatusChangedEvent) error {
	meta := order.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	now := time.Now()
	upd := s.client.POSOrder.UpdateOneID(order.ID)
	switch status {
	case "cancelled", "refunded":
		if order.Status == StatusCancelled || order.Status == StatusVoided || order.Status == StatusCompleted {
			return nil
		}
		if reason, _ := evt.Data["reason"].(string); reason != "" {
			meta["cancel_reason"] = reason
		}
		meta["cancelled_online_at"] = now.Format(time.RFC3339)
		upd = upd.SetStatus(StatusCancelled)
	case "out_for_delivery":
		if order.Status == StatusCancelled || order.Status == StatusVoided {
			return nil
		}
		meta["dispatch_status"] = "out_for_delivery"
		meta["left_counter_at"] = now.Format(time.RFC3339)
	case "delivered", "completed":
		if collected, _ := meta["collected"].(bool); collected {
			return nil
		}
		if order.Status == StatusCancelled || order.Status == StatusVoided {
			return nil
		}
		meta["collected"] = true
		meta["collected_at"] = now.Format(time.RFC3339)
		if status == "delivered" {
			meta["dispatch_status"] = "delivered"
		}
		upd = upd.SetStatus(StatusCompleted)
	default:
		return nil
	}
	if _, err := upd.SetMetadata(meta).Save(ctx); err != nil {
		return fmt.Errorf("mirror %s onto POS order %s: %w", status, order.ID, err)
	}

	// Close the order's open KDS tickets: voided for a cancellation (the food was never handed
	// over), served once it left the outlet.
	to := kdsticket.StatusServed
	if status == "cancelled" || status == "refunded" {
		to = kdsticket.StatusVoided
	}
	s.closeOpenTickets(ctx, order, to)
	s.logger.Info("online order lifecycle mirrored onto POS",
		zap.String("pos_order_id", order.ID.String()), zap.String("status", status))
	return nil
}

// closeOpenTickets moves an order's still-open KDS tickets to a terminal status and pushes the
// change to the outlet's live boards.
func (s *KDSOrderingSubscriber) closeOpenTickets(ctx context.Context, order *ent.POSOrder, to kdsticket.Status) {
	tickets, err := s.client.KDSTicket.Query().
		Where(
			kdsticket.OrderID(order.ID),
			kdsticket.StatusIn(kdsticket.StatusPending, kdsticket.StatusInProgress, kdsticket.StatusReady),
		).
		All(ctx)
	if err != nil || len(tickets) == 0 {
		return
	}
	now := time.Now()
	ids := make([]uuid.UUID, 0, len(tickets))
	for _, t := range tickets {
		ids = append(ids, t.ID)
	}
	if _, err := s.client.KDSTicket.Update().
		Where(kdsticket.IDIn(ids...)).
		SetStatus(to).
		SetCompletedAt(now).
		Save(ctx); err != nil {
		s.logger.Warn("online lifecycle: failed to close KDS tickets", zap.Error(err))
		return
	}
	if s.kdsHub == nil {
		return
	}
	for _, t := range tickets {
		s.kdsHub.BroadcastToOutlet(order.TenantID, order.OutletID, kdsmod.Message{
			Type: "ticket_updated",
			Payload: map[string]any{
				"ticket_id":    t.ID,
				"order_id":     order.ID,
				"order_number": order.OrderNumber,
				"station_id":   t.StationID,
				"status":       string(to),
				"completed_at": now,
			},
		})
	}
}

// kitchenAlreadyStarted reports whether the outlet itself already started any ticket of the
// order. ordering's "preparing" status is usually the echo of that start (KDS Start publishes
// pos.online_order.preparing), so it must not flip the order's OTHER stations' tickets too.
func (s *KDSOrderingSubscriber) kitchenAlreadyStarted(ctx context.Context, orderID uuid.UUID) bool {
	n, err := s.client.KDSTicket.Query().
		Where(
			kdsticket.OrderID(orderID),
			kdsticket.StatusIn(kdsticket.StatusInProgress, kdsticket.StatusReady, kdsticket.StatusServed),
		).
		Count(ctx)
	return err == nil && n > 0
}

// onlinePOSOrder loads the POS order linked to an online order id (nil when none).
func (s *KDSOrderingSubscriber) onlinePOSOrder(ctx context.Context, externalID string) *ent.POSOrder {
	link, err := s.client.OrderLink.Query().
		Where(orderlink.ExternalOrderID(externalID), orderlink.ChannelSourceIn(channelClickAndCollect, channelDelivery)).
		First(ctx)
	if err != nil {
		return nil
	}
	o, err := s.client.POSOrder.Query().Where(posorder.ID(link.OrderID)).Only(ctx)
	if err != nil {
		return nil
	}
	return o
}
