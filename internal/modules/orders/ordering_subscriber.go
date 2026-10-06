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
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	kdsmod "github.com/bengobox/pos-service/internal/modules/kds"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// orderingStatusChangedEvent is the envelope for ordering.order.status.changed.
// ordering-backend now publishes the fleet-uniform shared-events envelope
// (event_type/tenant_id/payload), so the wrapper decodes those field names.
type orderingStatusChangedEvent struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"event_type"`
	TenantID string                 `json:"tenant_id"`
	Data     map[string]interface{} `json:"payload"`
}

// KDSOrderingSubscriber keeps the POS copy of an online order in step with status changes made in
// ordering (staff dashboard, scheduler, customer): it mirrors lifecycle statuses
// (ordering_lifecycle.go), makes sure a confirmed order has its tickets, and starts them when
// ordering marks the order preparing. Tickets are only ever created through the orders service
// (issueKDSTickets), so the outlet's use case, printer-only switch and order type always apply.
type KDSOrderingSubscriber struct {
	client    *ent.Client
	svc       *Service
	logger    *zap.Logger
	publisher *events.Publisher
	kdsHub    *kdsmod.Hub
	// hasFeature gates KDS-ticket sync by subscription entitlement. When set, KDS
	// tickets are only created for tenants entitled to the kds feature. Nil → fail open.
	hasFeature func(ctx context.Context, tenantID, feature string) bool
}

// SetKDSHub wires the WebSocket hub so ticket changes broadcast immediately.
func (s *KDSOrderingSubscriber) SetKDSHub(h *kdsmod.Hub) { s.kdsHub = h }

// SetOrderService wires the orders service, the single place tickets are created.
func (s *KDSOrderingSubscriber) SetOrderService(svc *Service) { s.svc = svc }

// SetFeatureGate wires the subscription entitlement check used to gate KDS sync.
func (s *KDSOrderingSubscriber) SetFeatureGate(fn func(ctx context.Context, tenantID, feature string) bool) {
	s.hasFeature = fn
}

// NewKDSOrderingSubscriber creates a new KDS subscriber for ordering service events.
func NewKDSOrderingSubscriber(client *ent.Client, logger *zap.Logger) *KDSOrderingSubscriber {
	return &KDSOrderingSubscriber{
		client: client,
		logger: logger.Named("pos.kds_ordering_subscriber"),
	}
}

// SetPublisher sets the event publisher.
func (s *KDSOrderingSubscriber) SetPublisher(p *events.Publisher) {
	s.publisher = p
}

// SubscribeToOrderingEvents subscribes to ordering.order.status.changed via JetStream.
func (s *KDSOrderingSubscriber) SubscribeToOrderingEvents(nc *nats.Conn) error {
	if nc == nil {
		return fmt.Errorf("kds ordering subscriber: NATS connection is nil")
	}

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("kds ordering subscriber: jetstream init: %w", err)
	}

	sharedevents.SubscribeQueueWithRebind(s.logger, js, "ordering", "ordering.order.status.changed", "pos-kds-order-events", func(msg *nats.Msg) {
		var evt orderingStatusChangedEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			s.logger.Error("kds: failed to unmarshal ordering status event", zap.Error(err))
			_ = msg.Ack()
			return
		}

		newStatus, _ := evt.Data["new_status"].(string)
		ctx := context.Background()
		if isLifecycleStatus(newStatus) {
			if err := s.applyOnlineLifecycle(ctx, &evt, newStatus); err != nil {
				s.logger.Error("online lifecycle: failed to mirror status", zap.String("new_status", newStatus), zap.Error(err))
				_ = msg.Nak()
				return
			}
			_ = msg.Ack()
			return
		}
		if newStatus != "confirmed" && newStatus != "preparing" {
			_ = msg.Ack()
			return
		}
		if newStatus == "preparing" {
			if orderID, _ := evt.Data["order_id"].(string); orderID != "" {
				if o := s.onlinePOSOrder(ctx, orderID); o != nil && s.kitchenAlreadyStarted(ctx, o.ID) {
					_ = msg.Ack() // echo of our own KDS start; leave the other stations alone
					return
				}
			}
		}

		if err := s.handleStatusChanged(ctx, &evt, newStatus); err != nil {
			s.logger.Error("kds: failed to handle ordering status change",
				zap.String("event_id", evt.ID),
				zap.String("new_status", newStatus),
				zap.Error(err),
			)
			_ = msg.Nak()
			return
		}

		_ = msg.Ack()
	},
		nats.BindStream("ordering"),
		nats.Durable("pos-kds-order-events"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(5),
	)

	s.logger.Info("kds ordering subscriber started", zap.String("subject", "ordering.order.status.changed"))
	return nil
}

// handleStatusChanged makes sure a confirmed or preparing online order has its tickets (issued
// through the orders service, deduplicated per station) and, for preparing, starts the ones still
// pending. A held order waits for ordering.order.confirmed (ConfirmedOrderConsumer); an early
// acceptance of a scheduled order is only recorded.
func (s *KDSOrderingSubscriber) handleStatusChanged(ctx context.Context, evt *orderingStatusChangedEvent, newStatus string) error {
	orderIDStr, _ := evt.Data["order_id"].(string)
	tenantIDStr := evt.TenantID
	if v, ok := evt.Data["tenant_id"].(string); ok && v != "" {
		tenantIDStr = v
	}
	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		return fmt.Errorf("invalid tenant_id: %w", err)
	}
	if s.hasFeature != nil && !s.hasFeature(ctx, tenantID.String(), "kds") {
		return nil
	}

	posOrder := s.onlinePOSOrder(ctx, orderIDStr)
	if posOrder == nil || posOrder.TenantID != tenantID {
		return nil // not ingested into POS (ordering has other channels too)
	}

	if posOrder.Status == StatusAwaitingAcceptance {
		if newStatus == "confirmed" {
			meta := posOrder.Metadata
			if meta == nil {
				meta = map[string]any{}
			}
			if _, done := meta["accepted_at"]; !done {
				meta["accepted_at"] = time.Now().Format(time.RFC3339)
				_ = s.client.POSOrder.UpdateOneID(posOrder.ID).SetMetadata(meta).Exec(ctx)
			}
		}
		return nil
	}
	if posOrder.Status != StatusOpen || s.svc == nil {
		return nil
	}

	lines, err := s.client.POSOrderLine.Query().
		Where(posorderline.OrderID(posOrder.ID)).
		WithModifiers().
		All(ctx)
	if err != nil {
		return fmt.Errorf("query order lines: %w", err)
	}
	if err := s.svc.issueKDSTickets(ctx, tenantID, posOrder, lines, kdsIssue{firstRound: true}); err != nil {
		return fmt.Errorf("issue tickets: %w", err)
	}
	if newStatus == "preparing" {
		s.startPendingTickets(ctx, posOrder)
	}

	if s.publisher != nil {
		_ = s.publisher.PublishKDSOrderUpdated(ctx, tenantID, map[string]any{
			"external_order_id": orderIDStr,
			"order_number":      posOrder.OrderNumber,
			"pos_order_id":      posOrder.ID.String(),
			"new_status":        newStatus,
		})
	}
	return nil
}

// startPendingTickets moves an order's pending tickets to in progress when ordering marks the order
// preparing, and pushes the change to the outlet's boards.
func (s *KDSOrderingSubscriber) startPendingTickets(ctx context.Context, order *ent.POSOrder) {
	tickets, err := s.client.KDSTicket.Query().
		Where(kdsticket.OrderID(order.ID), kdsticket.StatusEQ(kdsticket.StatusPending)).
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
		Where(kdsticket.IDIn(ids...), kdsticket.StatusEQ(kdsticket.StatusPending)).
		SetStatus(kdsticket.StatusInProgress).
		SetStartedAt(now).
		Save(ctx); err != nil {
		s.logger.Warn("kds: failed to start tickets for preparing order", zap.Error(err))
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
				"status":       string(kdsticket.StatusInProgress),
			},
		})
	}
}
