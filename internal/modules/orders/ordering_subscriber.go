package orders

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/kdsstation"
	"github.com/bengobox/pos-service/internal/ent/kdsticket"
	"github.com/bengobox/pos-service/internal/ent/orderlink"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// orderingStatusChangedEvent is the envelope for ordering.order.status.changed.
type orderingStatusChangedEvent struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	TenantID string                 `json:"tenantId"`
	Data     map[string]interface{} `json:"data"`
}

// KDSOrderingSubscriber subscribes to ordering.order.status.changed to create KDS tickets.
type KDSOrderingSubscriber struct {
	client    *ent.Client
	logger    *zap.Logger
	publisher *events.Publisher
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

	_, err = js.Subscribe("ordering.order.status.changed", func(msg *nats.Msg) {
		var evt orderingStatusChangedEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			s.logger.Error("kds: failed to unmarshal ordering status event", zap.Error(err))
			_ = msg.Ack()
			return
		}

		newStatus, _ := evt.Data["new_status"].(string)
		if newStatus != "confirmed" && newStatus != "preparing" {
			_ = msg.Ack()
			return
		}

		ctx := context.Background()
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
	if err != nil {
		return fmt.Errorf("kds ordering subscriber: subscribe: %w", err)
	}

	s.logger.Info("kds ordering subscriber started", zap.String("subject", "ordering.order.status.changed"))
	return nil
}

func (s *KDSOrderingSubscriber) handleStatusChanged(ctx context.Context, evt *orderingStatusChangedEvent, newStatus string) error {
	orderIDStr, _ := evt.Data["order_id"].(string)
	orderNumber, _ := evt.Data["order_number"].(string)

	tenantIDStr := evt.TenantID
	if v, ok := evt.Data["tenant_id"].(string); ok && v != "" {
		tenantIDStr = v
	}

	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		return fmt.Errorf("invalid tenant_id: %w", err)
	}

	// Look up the POS order linked to this external ordering order
	link, err := s.client.OrderLink.Query().
		Where(orderlink.ExternalOrderID(orderIDStr)).
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			s.logger.Debug("kds: no POS order linked to ordering order, skipping",
				zap.String("external_order_id", orderIDStr))
			return nil
		}
		return fmt.Errorf("query order link: %w", err)
	}

	posOrder, err := s.client.POSOrder.Get(ctx, link.OrderID)
	if err != nil {
		return fmt.Errorf("get POS order: %w", err)
	}

	// Fetch order lines to build KDS item payload
	lines, err := s.client.POSOrderLine.Query().
		Where(posorderline.OrderID(posOrder.ID)).
		All(ctx)
	if err != nil {
		return fmt.Errorf("query order lines: %w", err)
	}

	items := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		items = append(items, map[string]any{
			"sku":        l.Sku,
			"name":       l.Name,
			"quantity":   l.Quantity,
			"unit_price": l.UnitPrice,
		})
	}

	// Find active KDS stations for the outlet
	stations, err := s.client.KDSStation.Query().
		Where(
			kdsstation.TenantID(tenantID),
			kdsstation.OutletID(posOrder.OutletID),
			kdsstation.IsActive(true),
		).
		All(ctx)
	if err != nil {
		return fmt.Errorf("query KDS stations: %w", err)
	}

	if len(stations) == 0 {
		s.logger.Debug("kds: no active stations for outlet, skipping",
			zap.String("outlet_id", posOrder.OutletID.String()))
		return nil
	}

	// Parse table reference from metadata for display on the KDS screen.
	tableRef := ""
	if v, ok := posOrder.Metadata["table_number"].(string); ok {
		tableRef = v
	}
	if tableRef == "" {
		if v, ok := posOrder.Metadata["table_name"].(string); ok {
			tableRef = v
		}
	}

	for _, station := range stations {
		if err := s.upsertKDSTicket(ctx, tenantID, station.ID, posOrder.ID, orderNumber, newStatus, tableRef, items); err != nil {
			s.logger.Error("kds: failed to upsert ticket",
				zap.String("station_id", station.ID.String()),
				zap.Error(err))
		}
	}

	// Publish KDS order updated event for real-time UI refresh
	if s.publisher != nil {
		_ = s.publisher.PublishKDSOrderUpdated(ctx, tenantID, map[string]any{
			"external_order_id": orderIDStr,
			"order_number":      orderNumber,
			"pos_order_id":      posOrder.ID.String(),
			"new_status":        newStatus,
			"station_count":     len(stations),
		})
	}

	s.logger.Info("kds tickets upserted for ordering event",
		zap.String("external_order_id", orderIDStr),
		zap.String("new_status", newStatus),
		zap.Int("stations", len(stations)),
	)
	return nil
}

func (s *KDSOrderingSubscriber) upsertKDSTicket(
	ctx context.Context,
	tenantID, stationID, posOrderID uuid.UUID,
	orderNumber, newStatus, tableRef string,
	items []map[string]any,
) error {
	existing, err := s.client.KDSTicket.Query().
		Where(
			kdsticket.OrderID(posOrderID),
			kdsticket.StationID(stationID),
		).
		First(ctx)

	if err != nil && !ent.IsNotFound(err) {
		return fmt.Errorf("query existing KDS ticket: %w", err)
	}

	if ent.IsNotFound(err) {
		// Create new ticket
		ticketStatus := kdsticket.StatusPending
		if newStatus == "preparing" {
			ticketStatus = kdsticket.StatusInProgress
		}
		c := s.client.KDSTicket.Create().
			SetTenantID(tenantID).
			SetStationID(stationID).
			SetOrderID(posOrderID).
			SetOrderNumber(orderNumber).
			SetStatus(ticketStatus).
			SetItems(items)
		if tableRef != "" {
			c = c.SetTableReference(tableRef)
		}
		_, err = c.Save(ctx)
		return err
	}

	// Update existing ticket status if moving to preparing
	if newStatus == "preparing" && existing.Status == kdsticket.StatusPending {
		now := time.Now()
		_, err = existing.Update().
			SetStatus(kdsticket.StatusInProgress).
			SetStartedAt(now).
			Save(ctx)
		return err
	}

	return nil
}
