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
	"github.com/bengobox/pos-service/internal/platform/events"
)

// OrderForPickupEvent represents the ordering.order.for_pickup event payload.
type OrderForPickupEvent struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	TenantID string                 `json:"tenantId"`
	Data     map[string]interface{} `json:"data"`
}

// PickupItemData holds parsed item data from the pickup event.
type PickupItemData struct {
	SKU       string  `json:"sku"`
	Name      string  `json:"name"`
	Quantity  float64 `json:"quantity"`
	UnitPrice float64 `json:"unit_price"`
}

// PickupConsumer handles incoming click-and-collect orders from ordering-service.
type PickupConsumer struct {
	client    *ent.Client
	orderSvc  *Service
	logger    *zap.Logger
	publisher *events.Publisher
}

// NewPickupConsumer creates a new click-and-collect order consumer.
func NewPickupConsumer(client *ent.Client, orderSvc *Service, logger *zap.Logger) *PickupConsumer {
	return &PickupConsumer{
		client:   client,
		orderSvc: orderSvc,
		logger:   logger.Named("pos.pickup_consumer"),
	}
}

// SetPublisher sets the event publisher for order lifecycle events.
func (c *PickupConsumer) SetPublisher(p *events.Publisher) {
	c.publisher = p
}

// SubscribeToPickupOrders subscribes to ordering.order.for_pickup via JetStream.
func (c *PickupConsumer) SubscribeToPickupOrders(nc *nats.Conn) error {
	if nc == nil {
		return fmt.Errorf("pickup consumer: NATS connection is nil")
	}

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("pickup consumer: jetstream init: %w", err)
	}

	_, err = js.Subscribe("ordering.order.for_pickup", func(msg *nats.Msg) {
		var evt OrderForPickupEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			c.logger.Error("pickup consumer: failed to unmarshal event", zap.Error(err))
			_ = msg.Ack() // unrecoverable parse error
			return
		}

		ctx := context.Background()
		if err := c.handleOrderForPickup(ctx, &evt); err != nil {
			c.logger.Error("pickup consumer: failed to handle order",
				zap.String("event_id", evt.ID),
				zap.Error(err),
			)
			_ = msg.Nak() // retry
			return
		}

		c.logger.Info("pickup consumer: order created for kitchen prep",
			zap.String("event_id", evt.ID),
		)
		_ = msg.Ack()
	},
		nats.BindStream("ordering"),
		nats.Durable("pos-pickup-orders"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(5),
	)
	if err != nil {
		return fmt.Errorf("pickup consumer: subscribe: %w", err)
	}

	c.logger.Info("pickup consumer started", zap.String("subject", "ordering.order.for_pickup"))
	return nil
}

// handleOrderForPickup creates a POS order linked to the online order.
func (c *PickupConsumer) handleOrderForPickup(ctx context.Context, evt *OrderForPickupEvent) error {
	data := evt.Data

	tenantIDStr, _ := data["tenant_id"].(string)
	if tenantIDStr == "" {
		tenantIDStr = evt.TenantID
	}
	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		return fmt.Errorf("invalid tenant_id: %w", err)
	}

	orderIDStr, _ := data["order_id"].(string)
	if orderIDStr == "" {
		return fmt.Errorf("missing order_id in event data")
	}

	orderNumber, _ := data["order_number"].(string)
	outletIDStr, _ := data["outlet_id"].(string)
	customerName, _ := data["customer_name"].(string)
	customerPhone, _ := data["customer_phone"].(string)

	outletID := uuid.Nil
	if outletIDStr != "" {
		outletID, _ = uuid.Parse(outletIDStr)
	}

	// Parse items
	var items []PickupItemData
	if rawItems, ok := data["items"]; ok {
		itemBytes, _ := json.Marshal(rawItems)
		_ = json.Unmarshal(itemBytes, &items)
	}

	// Build order lines
	lines := make([]OrderLineInput, 0, len(items))
	for _, item := range items {
		lines = append(lines, OrderLineInput{
			CatalogItemID: uuid.Nil, // No catalog mapping for online items
			SKU:           item.SKU,
			Name:          item.Name,
			Quantity:      item.Quantity,
			UnitPrice:     item.UnitPrice,
			TotalPrice:    item.UnitPrice * item.Quantity,
			TaxStatus:     "taxable",
		})
	}

	// Calculate totals
	totals := c.orderSvc.CalculateTotals(lines, decimal.Zero)

	// Use a prefixed order number for click-and-collect
	posOrderNumber := orderNumber
	if posOrderNumber == "" {
		posOrderNumber = c.orderSvc.GenerateOrderNumber()
	} else {
		posOrderNumber = "CC-" + posOrderNumber
	}

	// Create POS order in "open" status (ready for kitchen)
	tx, err := c.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Use a system device/user ID for online orders
	systemID := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	order, err := tx.POSOrder.Create().
		SetTenantID(tenantID).
		SetOutletID(outletID).
		SetDeviceID(systemID).
		SetUserID(systemID).
		SetOrderNumber(posOrderNumber).
		SetStatus(StatusOpen).
		SetSubtotal(totals.Subtotal.InexactFloat64()).
		SetTaxTotal(totals.TaxTotal.InexactFloat64()).
		SetDiscountTotal(totals.DiscountTotal.InexactFloat64()).
		SetTotalAmount(totals.TotalAmount.InexactFloat64()).
		SetCurrency(c.orderSvc.DefaultCurrency()).
		SetMetadata(map[string]any{
			"source":         "click_and_collect",
			"online_order_id": orderIDStr,
			"customer_name":   customerName,
			"customer_phone":  customerPhone,
		}).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("create POS order: %w", err)
	}

	// Create order lines
	for _, line := range lines {
		lineTotal := decimal.NewFromFloat(line.TotalPrice)
		if lineTotal.IsZero() {
			lineTotal = decimal.NewFromFloat(line.UnitPrice).Mul(decimal.NewFromFloat(line.Quantity))
		}
		_, err = tx.POSOrderLine.Create().
			SetOrderID(order.ID).
			SetCatalogItemID(line.CatalogItemID).
			SetSku(line.SKU).
			SetName(line.Name).
			SetQuantity(line.Quantity).
			SetUnitPrice(line.UnitPrice).
			SetTotalPrice(lineTotal.InexactFloat64()).
			Save(ctx)
		if err != nil {
			return fmt.Errorf("create order line: %w", err)
		}
	}

	// Create order link to track the online→POS mapping
	_, err = tx.OrderLink.Create().
		SetOrderID(order.ID).
		SetExternalOrderID(orderIDStr).
		SetChannelSource("ordering_click_and_collect").
		Save(ctx)
	if err != nil {
		return fmt.Errorf("create order link: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Publish pos.order.created event
	if c.publisher != nil {
		_ = c.publisher.PublishOrderCreated(ctx, tenantID, map[string]any{
			"order_id":        order.ID.String(),
			"order_number":    posOrderNumber,
			"outlet_id":       outletID.String(),
			"total_amount":    totals.TotalAmount.String(),
			"currency":        c.orderSvc.DefaultCurrency(),
			"item_count":      len(lines),
			"source":          "click_and_collect",
			"online_order_id": orderIDStr,
		})
	}

	c.logger.Info("click-and-collect POS order created",
		zap.String("pos_order_id", order.ID.String()),
		zap.String("online_order_id", orderIDStr),
		zap.String("order_number", posOrderNumber),
	)
	return nil
}
