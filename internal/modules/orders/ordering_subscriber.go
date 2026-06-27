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

// KDSOrderingSubscriber subscribes to ordering.order.status.changed to create KDS tickets.
type KDSOrderingSubscriber struct {
	client    *ent.Client
	logger    *zap.Logger
	publisher *events.Publisher
	kdsHub    *kdsmod.Hub
	// hasFeature gates KDS-ticket sync by subscription entitlement. When set, KDS
	// tickets are only created for tenants entitled to the kds feature. Nil → fail open.
	hasFeature func(ctx context.Context, tenantID, feature string) bool
}

// SetKDSHub wires the WebSocket hub so new KDS tickets broadcast immediately.
func (s *KDSOrderingSubscriber) SetKDSHub(h *kdsmod.Hub) { s.kdsHub = h }

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

	// Gate KDS-ticket sync by entitlement: only tenants on a plan that includes the
	// kds feature get tickets written into their POS schema. Fails open if no gate wired.
	if s.hasFeature != nil && !s.hasFeature(ctx, tenantID.String(), "kds") {
		s.logger.Debug("kds: tenant not entitled to kds — skipping ticket sync",
			zap.String("tenant_id", tenantID.String()))
		return nil
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

	// Route lines to stations using the same algorithm as POS orders.
	// Online orders may not have kds_station_id on lines (set to nil), so they
	// fall through to category_filter keyword matching then expo/all stations.
	stationItems := routeLinesToStations(lines, stations)
	tableRef := parseTableRef(posOrder)

	for _, station := range stations {
		items := stationItems[station.ID]
		if len(items) == 0 {
			continue // no items for this station — skip
		}
		if err := s.upsertKDSTicket(ctx, tenantID, posOrder.OutletID, station.ID, posOrder.ID, orderNumber, newStatus, tableRef, items); err != nil {
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
	tenantID, outletID, stationID, posOrderID uuid.UUID,
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
		ticket, err := c.Save(ctx)
		if err != nil {
			return err
		}
		if s.kdsHub != nil {
			s.kdsHub.BroadcastToOutlet(tenantID, outletID, kdsmod.Message{
				Type: "ticket_created",
				Payload: map[string]any{
					"ticket_id":       ticket.ID,
					"order_id":        posOrderID,
					"order_number":    orderNumber,
					"station_id":      stationID,
					"table_reference": tableRef,
					"status":          string(ticketStatus),
					"items":           items,
				},
			})
		}
		return nil
	}

	// Update existing ticket status if moving to preparing
	if newStatus == "preparing" && existing.Status == kdsticket.StatusPending {
		now := time.Now()
		updated, err := existing.Update().
			SetStatus(kdsticket.StatusInProgress).
			SetStartedAt(now).
			Save(ctx)
		if err != nil {
			return err
		}
		if s.kdsHub != nil {
			s.kdsHub.BroadcastToOutlet(tenantID, outletID, kdsmod.Message{
				Type: "ticket_updated",
				Payload: map[string]any{
					"ticket_id":    updated.ID,
					"order_id":     posOrderID,
					"order_number": orderNumber,
					"station_id":   stationID,
					"status":       string(kdsticket.StatusInProgress),
				},
			})
		}
		return nil
	}

	return nil
}
