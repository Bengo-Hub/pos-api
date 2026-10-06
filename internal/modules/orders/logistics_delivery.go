package orders

import (
	"context"
	"fmt"
	"strings"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/kdsticket"
	"github.com/bengobox/pos-service/internal/ent/posorder"
)

// POS-native delivery tracking. A delivery order rung up at the till is dispatched from the POS
// queue straight to logistics-api (OnlineOrderHandler.AssignRider / DispatchShipment), with the POS
// order id as the task's external reference and source_service "pos". logistics then publishes
// logistics.task.<status> as the rider works the job. This subscriber mirrors those statuses onto
// the POS order so the counter sees the rider's progress, the order leaves the dispatch queue once
// it is delivered and paid, and a failed or cancelled dispatch can be sent out again.
//
// Online deliveries are not handled here: ordering-backend owns them and the POS copy follows
// ordering's lifecycle events (ordering_lifecycle.go).

// Dispatch states stored in POS order metadata.dispatch_status, in the order a delivery moves.
const (
	DispatchDispatched     = "dispatched"
	DispatchRiderAssigned  = "rider_assigned"
	DispatchRiderArriving  = "rider_arriving"
	DispatchOutForDelivery = "out_for_delivery"
	DispatchDelivered      = "delivered"
	DispatchFailed         = "delivery_failed"
	DispatchCancelled      = "dispatch_cancelled"
)

var dispatchRank = map[string]int{
	"":                     0,
	DispatchFailed:         0,
	DispatchCancelled:      0,
	DispatchDispatched:     1,
	DispatchRiderAssigned:  2,
	DispatchRiderArriving:  3,
	DispatchOutForDelivery: 4,
	DispatchDelivered:      5,
}

// dispatchStateFor maps a logistics task status onto the POS dispatch state ("" = not mirrored).
func dispatchStateFor(taskStatus string) string {
	switch taskStatus {
	case "created", "pending", "unassigned":
		return DispatchDispatched
	case "assigned", "accepted":
		return DispatchRiderAssigned
	case "en_route_pickup", "arrived_pickup":
		return DispatchRiderArriving
	case "picked_up", "en_route", "en_route_dropoff", "arrived_dropoff":
		return DispatchOutForDelivery
	case "delivered", "completed":
		return DispatchDelivered
	case "failed":
		return DispatchFailed
	case "cancelled":
		return DispatchCancelled
	}
	return ""
}

// deliveryUpdate is what one logistics event changes on a POS delivery order.
type deliveryUpdate struct {
	meta     map[string]any
	complete bool // the order is delivered and paid: close it and move it to history
	changed  bool
}

// applyDeliveryEvent decides the POS-side effect of one logistics task event. It is pure so the
// ordering rules are tested without a database:
//   - progress never moves backwards (a late "en_route" after "delivered" is ignored);
//   - unassigned (rider declined or was taken off) returns the order to "dispatched" before pickup;
//   - failed and cancelled end this dispatch: the task reference is cleared so the counter can send
//     the order out again, unless it was already delivered;
//   - delivered records how the rider was paid at the door. The order is closed only when the till
//     already holds full payment; otherwise it stays on the queue as delivered with the cash the
//     rider owes, and the cashier settles it from the queue.
func applyDeliveryEvent(meta map[string]any, total, paid float64, taskStatus string, payload map[string]any, now time.Time) deliveryUpdate {
	out := map[string]any{}
	for k, v := range meta {
		out[k] = v
	}
	state := dispatchStateFor(taskStatus)
	if state == "" {
		return deliveryUpdate{meta: out}
	}
	current, _ := out["dispatch_status"].(string)
	if current == DispatchDelivered {
		return deliveryUpdate{meta: out} // terminal for this order
	}

	switch {
	case taskStatus == "unassigned":
		if dispatchRank[current] > dispatchRank[DispatchRiderArriving] {
			return deliveryUpdate{meta: out} // already left the outlet; a stale event
		}
		delete(out, "rider_id")
		delete(out, "rider_name")
		delete(out, "rider_phone")
	case state == DispatchFailed || state == DispatchCancelled:
		if id, _ := out["logistics_task_id"].(string); id != "" {
			out["ended_logistics_task_id"] = id
		}
		delete(out, "logistics_task_id")
		if r, _ := payload["reason"].(string); strings.TrimSpace(r) != "" {
			out["dispatch_reason"] = strings.TrimSpace(r)
		}
	default:
		if dispatchRank[state] < dispatchRank[current] {
			return deliveryUpdate{meta: out}
		}
	}

	out["dispatch_status"] = state
	out["dispatch_updated_at"] = now.Format(time.RFC3339)
	if v, _ := payload["fleet_member_id"].(string); v != "" && taskStatus != "unassigned" {
		out["rider_id"] = v
	}
	if v, _ := payload["rider_name"].(string); v != "" && taskStatus != "unassigned" {
		out["rider_name"] = v
	}
	if v, _ := payload["rider_phone"].(string); v != "" && taskStatus != "unassigned" {
		out["rider_phone"] = v
	}
	if state == DispatchOutForDelivery {
		if _, ok := out["left_counter_at"]; !ok {
			out["left_counter_at"] = now.Format(time.RFC3339)
		}
	}

	complete := false
	if state == DispatchDelivered {
		out["delivered_at"] = now.Format(time.RFC3339)
		if collected, _ := payload["cash_collected"].(bool); collected {
			out["cod_collected"] = true
			if amt, ok := payload["amount_collected"].(float64); ok {
				out["cod_amount_collected"] = amt
			}
			method, _ := payload["collection_method"].(string)
			if method == "" {
				method = "cash"
			}
			out["cod_method"] = method
			if ref, _ := payload["collection_reference"].(string); ref != "" {
				out["cod_reference"] = strings.ToUpper(strings.TrimSpace(ref))
			}
		}
		if total > 0 && paid+0.005 >= total || total == 0 {
			out["collected"] = true
			out["collected_at"] = now.Format(time.RFC3339)
			complete = true
		}
	}
	return deliveryUpdate{meta: out, complete: complete, changed: true}
}

// LogisticsDeliverySubscriber mirrors logistics task progress onto POS-native delivery orders.
type LogisticsDeliverySubscriber struct {
	client *ent.Client
	svc    *Service
	logger *zap.Logger
}

// NewLogisticsDeliverySubscriber creates the subscriber.
func NewLogisticsDeliverySubscriber(client *ent.Client, svc *Service, logger *zap.Logger) *LogisticsDeliverySubscriber {
	return &LogisticsDeliverySubscriber{client: client, svc: svc, logger: logger.Named("pos.logistics_delivery")}
}

// Subscribe consumes logistics.task.* on the logistics stream with one queue-group durable, so a
// task's events are handled by one pod at a time across replicas.
func (s *LogisticsDeliverySubscriber) Subscribe(nc *nats.Conn) error {
	if nc == nil {
		return fmt.Errorf("logistics delivery subscriber: NATS connection is nil")
	}
	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("logistics delivery subscriber: jetstream init: %w", err)
	}
	const subject, durable = "logistics.task.*", "pos-logistics-task-events"
	sharedevents.SubscribeQueueWithRebind(s.logger, js, "logistics", subject, durable, func(msg *nats.Msg) {
		evt, perr := sharedevents.FromJSON(msg.Data)
		if perr != nil {
			_ = msg.Ack() // unparseable: drop
			return
		}
		taskStatus := strings.TrimPrefix(msg.Subject, "logistics.task.")
		if err := s.handle(context.Background(), evt.TenantID, taskStatus, evt.Payload); err != nil {
			s.logger.Error("logistics delivery: handler failed", zap.String("subject", msg.Subject), zap.Error(err))
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	},
		nats.BindStream("logistics"),
		nats.Durable(durable),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(5),
	)
	s.logger.Info("logistics delivery subscriber started", zap.String("subject", subject))
	return nil
}

func (s *LogisticsDeliverySubscriber) handle(ctx context.Context, tenantID uuid.UUID, taskStatus string, payload map[string]any) error {
	src, _ := payload["source_service"].(string)
	if src != "" && src != "pos" {
		return nil // ordering and other sources follow their own owners
	}
	ref, _ := payload["external_reference"].(string)
	orderID, err := uuid.Parse(strings.TrimPrefix(ref, "order:"))
	if err != nil {
		return nil
	}
	order, err := s.client.POSOrder.Query().Where(posorder.ID(orderID), posorder.TenantID(tenantID)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil
		}
		return err
	}
	// The event must belong to this order's current dispatch: events from an older dispatch
	// (cancelled, then sent out again) are ignored, and an event without a source must match the
	// task the POS created before it is trusted.
	taskID, _ := payload["task_id"].(string)
	current, _ := order.Metadata["logistics_task_id"].(string)
	if current != "" && taskID != "" && current != taskID {
		return nil
	}
	if ended, _ := order.Metadata["ended_logistics_task_id"].(string); ended != "" && ended == taskID {
		return nil
	}
	if src == "" && (current == "" || current != taskID) {
		return nil
	}
	switch order.Status {
	case StatusCancelled, StatusVoided:
		return nil
	}

	upd := applyDeliveryEvent(order.Metadata, order.TotalAmount, order.PaidTotal, taskStatus, payload, time.Now())
	if !upd.changed {
		return nil
	}
	q := s.client.POSOrder.UpdateOneID(order.ID).SetMetadata(upd.meta)
	if upd.complete && order.Status != StatusCompleted {
		q = q.SetStatus(StatusCompleted)
	}
	if _, err := q.Save(ctx); err != nil {
		return fmt.Errorf("mirror logistics %s onto POS order %s: %w", taskStatus, order.ID, err)
	}
	// Once it has left the outlet, nothing for this order belongs on a kitchen board.
	if st := upd.meta["dispatch_status"]; st == DispatchOutForDelivery || st == DispatchDelivered {
		closeOrderKDSTickets(ctx, s.client, s.svc.kdsHub, s.logger, tenantID, order.ID, kdsticket.StatusServed)
	}
	s.logger.Info("logistics delivery mirrored onto POS",
		zap.String("pos_order_id", order.ID.String()), zap.String("task_status", taskStatus))
	return nil
}
