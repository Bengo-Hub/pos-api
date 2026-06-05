package orders

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entorderlink "github.com/bengobox/pos-service/internal/ent/orderlink"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// ConfirmedOrderEvent is the envelope for ordering.order.confirmed.
// ordering-backend publishes its native {id,type,tenantId,data} envelope (NOT the
// shared-events event_type/payload envelope), so it is decoded exactly like
// orderingStatusChangedEvent / OrderForPickupEvent.
type ConfirmedOrderEvent struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	TenantID string                 `json:"tenantId"`
	Data     map[string]interface{} `json:"data"`
}

// confirmedItemData holds a single line item from the confirmed-order payload.
type confirmedItemData struct {
	SKU       string  `json:"sku"`
	Name      string  `json:"name"`
	Quantity  float64 `json:"quantity"`
	UnitPrice float64 `json:"unit_price"`
}

// ConfirmedOrderConsumer is the SINGLE online-order ingestion path. It consumes
// ordering.order.confirmed for BOTH pickup (click-and-collect) and delivery online
// orders, idempotently creating the POSOrder + lines + OrderLink and routing KDS
// tickets to the correct stations (reusing Service.createKDSTicketsForOrder).
type ConfirmedOrderConsumer struct {
	client    *ent.Client
	orderSvc  *Service
	logger    *zap.Logger
	publisher *events.Publisher
}

// NewConfirmedOrderConsumer creates a consumer for ordering.order.confirmed.
func NewConfirmedOrderConsumer(client *ent.Client, orderSvc *Service, logger *zap.Logger) *ConfirmedOrderConsumer {
	return &ConfirmedOrderConsumer{
		client:   client,
		orderSvc: orderSvc,
		logger:   logger.Named("pos.confirmed_consumer"),
	}
}

// SetPublisher wires the event publisher for pos.order.created emission.
func (c *ConfirmedOrderConsumer) SetPublisher(p *events.Publisher) { c.publisher = p }

// SubscribeToConfirmedOrders subscribes to ordering.order.confirmed via JetStream
// by binding the existing "ordering" stream (mirrors pickup_consumer / ordering_subscriber).
func (c *ConfirmedOrderConsumer) SubscribeToConfirmedOrders(nc *nats.Conn) error {
	if nc == nil {
		return fmt.Errorf("confirmed consumer: NATS connection is nil")
	}

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("confirmed consumer: jetstream init: %w", err)
	}

	_, err = js.Subscribe("ordering.order.confirmed", func(msg *nats.Msg) {
		var evt ConfirmedOrderEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			c.logger.Error("confirmed consumer: failed to unmarshal event", zap.Error(err))
			_ = msg.Ack() // unrecoverable parse error — drop
			return
		}

		ctx := context.Background()
		if err := c.handleOrderConfirmed(ctx, &evt); err != nil {
			c.logger.Error("confirmed consumer: failed to handle order",
				zap.String("event_id", evt.ID),
				zap.Error(err),
			)
			_ = msg.Nak() // retry
			return
		}
		_ = msg.Ack()
	},
		nats.BindStream("ordering"),
		nats.Durable("pos-confirmed-orders"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(5),
	)
	if err != nil {
		return fmt.Errorf("confirmed consumer: subscribe: %w", err)
	}

	c.logger.Info("confirmed consumer started", zap.String("subject", "ordering.order.confirmed"))
	return nil
}

// fulfillmentRouting maps fulfillment_type to the POS source/subtype/channel triple.
func fulfillmentRouting(fulfillmentType string) (source, subtype, channelSource string) {
	if fulfillmentType == "delivery" {
		return "online_delivery", "delivery", "ordering_delivery"
	}
	// default to pickup / click-and-collect for "pickup" and any unknown value
	return "click_and_collect", "takeaway", "ordering_click_and_collect"
}

// handleOrderConfirmed idempotently ingests a confirmed online order into POS.
func (c *ConfirmedOrderConsumer) handleOrderConfirmed(ctx context.Context, evt *ConfirmedOrderEvent) error {
	data := evt.Data

	orderIDStr, _ := data["order_id"].(string)
	if orderIDStr == "" {
		return fmt.Errorf("missing order_id in event data")
	}

	// Idempotency: skip if a POS order is already linked to this external order
	// (regardless of channel). A redelivery after a lost Ack — or a stale
	// ordering.order.for_pickup event handled first — must NOT create a duplicate.
	if exists, _ := c.client.OrderLink.Query().
		Where(entorderlink.ExternalOrderID(orderIDStr)).
		Exist(ctx); exists {
		c.logger.Info("confirmed consumer: online order already linked, skipping duplicate",
			zap.String("external_order_id", orderIDStr))
		return nil
	}

	tenantIDStr, _ := data["tenant_id"].(string)
	if tenantIDStr == "" {
		tenantIDStr = evt.TenantID
	}
	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		return fmt.Errorf("invalid tenant_id: %w", err)
	}

	outletID := uuid.Nil
	if outletIDStr, _ := data["outlet_id"].(string); outletIDStr != "" {
		outletID, _ = uuid.Parse(outletIDStr)
	}

	fulfillmentType, _ := data["fulfillment_type"].(string)
	source, subtype, channelSource := fulfillmentRouting(fulfillmentType)

	orderNumber, _ := data["order_number"].(string)
	customerName, _ := data["customer_name"].(string)
	customerEmail, _ := data["customer_email"].(string)
	customerPhone, _ := data["customer_phone"].(string)

	// Parse items.
	var items []confirmedItemData
	if rawItems, ok := data["items"]; ok {
		itemBytes, _ := json.Marshal(rawItems)
		_ = json.Unmarshal(itemBytes, &items)
	}

	// Build order lines (price field is total_price = unit_price * quantity).
	lines := make([]OrderLineInput, 0, len(items))
	for _, item := range items {
		lines = append(lines, OrderLineInput{
			CatalogItemID: uuid.Nil, // online items carry no local catalog mapping
			SKU:           item.SKU,
			Name:          item.Name,
			Quantity:      item.Quantity,
			UnitPrice:     item.UnitPrice,
			TotalPrice:    item.UnitPrice * item.Quantity,
			TaxStatus:     "taxable",
		})
	}

	totals := c.orderSvc.CalculateTotals(lines, decimal.Zero)

	// Prefix the POS order number by channel for at-a-glance identification.
	posOrderNumber := orderNumber
	if posOrderNumber == "" {
		posOrderNumber = c.orderSvc.GenerateOrderNumber()
	} else if fulfillmentType == "delivery" {
		posOrderNumber = "DL-" + posOrderNumber
	} else {
		posOrderNumber = "CC-" + posOrderNumber
	}

	tx, err := c.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// System device/user identity for machine-ingested online orders.
	systemID := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	// Create the POS order in "open" status so it is treated as live, with the
	// fulfillment-appropriate order_subtype. KDS ticket creation is triggered
	// explicitly below via the shared Service.createKDSTicketsForOrder.
	order, err := tx.POSOrder.Create().
		SetTenantID(tenantID).
		SetOutletID(outletID).
		SetDeviceID(systemID).
		SetUserID(systemID).
		SetOrderNumber(posOrderNumber).
		SetStatus(StatusOpen).
		SetOrderSubtype(posorder.OrderSubtype(subtype)).
		SetSubtotal(totals.Subtotal.InexactFloat64()).
		SetTaxTotal(totals.TaxTotal.InexactFloat64()).
		SetDiscountTotal(totals.DiscountTotal.InexactFloat64()).
		SetTotalAmount(totals.TotalAmount.InexactFloat64()).
		SetCurrency(c.orderSvc.DefaultCurrency()).
		SetMetadata(map[string]any{
			"source":           source,
			"order_subtype":    subtype,
			"fulfillment_type": fulfillmentType,
			"online_order_id":  orderIDStr,
			"customer_name":    customerName,
			"customer_email":   customerEmail,
			"customer_phone":   customerPhone,
		}).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("create POS order: %w", err)
	}

	for _, line := range lines {
		lineTotal := decimal.NewFromFloat(line.TotalPrice)
		if lineTotal.IsZero() {
			lineTotal = decimal.NewFromFloat(line.UnitPrice).Mul(decimal.NewFromFloat(line.Quantity))
		}
		if _, err = tx.POSOrderLine.Create().
			SetOrderID(order.ID).
			SetCatalogItemID(line.CatalogItemID).
			SetSku(line.SKU).
			SetName(line.Name).
			SetQuantity(line.Quantity).
			SetUnitPrice(line.UnitPrice).
			SetTotalPrice(lineTotal.InexactFloat64()).
			Save(ctx); err != nil {
			return fmt.Errorf("create order line: %w", err)
		}
	}

	// OrderLink records the online→POS mapping and is the idempotency key for
	// redeliveries and for the now-no-op ordering.order.for_pickup consumer.
	if _, err = tx.OrderLink.Create().
		SetOrderID(order.ID).
		SetExternalOrderID(orderIDStr).
		SetChannelSource(channelSource).
		Save(ctx); err != nil {
		return fmt.Errorf("create order link: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Create KDS tickets routed to the correct stations (reuses the same routing
	// + upsert logic POS-native open orders use — no duplicate ticket logic here).
	if ktErr := c.orderSvc.createKDSTicketsForOrder(ctx, tenantID, order); ktErr != nil {
		c.logger.Warn("confirmed consumer: KDS ticket creation failed",
			zap.String("pos_order_id", order.ID.String()), zap.Error(ktErr))
	}

	if c.publisher != nil {
		_ = c.publisher.PublishOrderCreated(ctx, tenantID, map[string]any{
			"order_id":         order.ID.String(),
			"order_number":     posOrderNumber,
			"outlet_id":        outletID.String(),
			"total_amount":     totals.TotalAmount.String(),
			"currency":         c.orderSvc.DefaultCurrency(),
			"item_count":       len(lines),
			"source":           source,
			"fulfillment_type": fulfillmentType,
			"online_order_id":  orderIDStr,
		})
	}

	c.logger.Info("confirmed online order ingested into POS",
		zap.String("pos_order_id", order.ID.String()),
		zap.String("online_order_id", orderIDStr),
		zap.String("order_number", posOrderNumber),
		zap.String("fulfillment_type", fulfillmentType),
		zap.String("source", source),
	)
	return nil
}
