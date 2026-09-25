package orders

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entappointment "github.com/bengobox/pos-service/internal/ent/appointment"
	entorderlink "github.com/bengobox/pos-service/internal/ent/orderlink"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// ConfirmedOrderEvent is the envelope for ordering.order.confirmed.
// ordering-backend now publishes the fleet-uniform shared-events envelope
// (event_type/tenant_id/payload), decoded exactly like orderingStatusChangedEvent /
// OrderForPickupEvent.
type ConfirmedOrderEvent struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"event_type"`
	TenantID string                 `json:"tenant_id"`
	Data     map[string]interface{} `json:"payload"`
}

// confirmedItemData holds a single line item from the confirmed-order payload.
type confirmedItemData struct {
	SKU        string                 `json:"sku"`
	Name       string                 `json:"name"`
	Quantity   float64                `json:"quantity"`
	UnitPrice  float64                `json:"unit_price"`
	TotalPrice float64                `json:"total_price"`
	Category   string                 `json:"category"`
	Notes      string                 `json:"notes"`
	Modifiers  []map[string]any       `json:"modifiers"`
	Metadata   map[string]interface{} `json:"metadata"`
}

// isService reports whether the line is a service booking (salon, barber, garage, printing)
// rather than a product to prepare or pack.
func (i confirmedItemData) isService() bool {
	if i.Metadata == nil {
		return false
	}
	if b, _ := i.Metadata["is_service"].(bool); b {
		return true
	}
	return false
}

// Channel sources recorded on OrderLink for each kind of outlet record an online order creates.
const (
	channelClickAndCollect = "ordering_click_and_collect"
	channelDelivery        = "ordering_delivery"
	channelAppointment     = "ordering_appointment"
)

// onlineOrderSystemID is the device/user identity stamped on machine-ingested online orders.
var onlineOrderSystemID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// ConfirmedOrderConsumer is the SINGLE online-order ingestion path. It consumes
// ordering.order.confirmed for pickup (click-and-collect) and delivery online orders:
//   - product lines become a POS order created through the same Service.CreateOrder the till
//     uses, so category/station routing, modifiers, KDS tickets and kitchen chits behave exactly
//     like a POS-native takeaway or delivery order;
//   - service lines (salon, barber, garage...) become confirmed calendar appointments.
//
// Both are idempotent: the POS order through CreateOrder's client reference, the appointments
// through their OrderLink rows.
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

// SetPublisher wires the event publisher (kept for wiring compatibility; order creation events
// are published by Service.CreateOrder itself).
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

	subscribe := func(subject, durable string, hold bool) {
		sharedevents.SubscribeQueueWithRebind(c.logger, js, "ordering", subject, durable, func(msg *nats.Msg) {
			var evt ConfirmedOrderEvent
			if err := json.Unmarshal(msg.Data, &evt); err != nil {
				c.logger.Error("confirmed consumer: failed to unmarshal event", zap.Error(err))
				_ = msg.Ack() // unrecoverable parse error — drop
				return
			}

			ctx := context.Background()
			if err := c.handleOrderHandoff(ctx, &evt, hold); err != nil {
				c.logger.Error("confirmed consumer: failed to handle order",
					zap.String("event_id", evt.ID),
					zap.String("subject", subject),
					zap.Error(err),
				)
				_ = msg.Nak() // retry
				return
			}
			_ = msg.Ack()
		},
			nats.BindStream("ordering"),
			nats.Durable(durable),
			nats.ManualAck(),
			nats.AckWait(30*time.Second),
			nats.MaxDeliver(5),
		)
		c.logger.Info("online order consumer started", zap.String("subject", subject), zap.Bool("hold", hold))
	}
	// Confirmed: the kitchen gets it (or a held order is released once accepted).
	subscribe("ordering.order.confirmed", "pos-confirmed-orders", false)
	// Awaiting acceptance (manual acceptance): the queue rings with Accept / Reject; no tickets yet.
	subscribe("ordering.order.awaiting_acceptance", "pos-awaiting-acceptance-orders", true)
	return nil
}

// fulfillmentRouting maps fulfillment_type to the POS source/subtype/channel triple.
func fulfillmentRouting(fulfillmentType string) (source, subtype, channelSource string) {
	if fulfillmentType == "delivery" {
		return "online_delivery", "delivery", channelDelivery
	}
	// default to pickup / click-and-collect for "pickup" and any unknown value
	return "click_and_collect", "takeaway", channelClickAndCollect
}

// onlineOrderClientReference is the CreateOrder idempotency key for an online order's POS record.
func onlineOrderClientReference(onlineOrderID string) string {
	return "online:" + onlineOrderID
}

// handleOrderConfirmed idempotently ingests a confirmed online order into POS.
func (c *ConfirmedOrderConsumer) handleOrderConfirmed(ctx context.Context, evt *ConfirmedOrderEvent) error {
	return c.handleOrderHandoff(ctx, evt, false)
}

// handleOrderHandoff ingests an online order into POS. hold=true (awaiting acceptance) creates the
// POS record and appointments on hold; hold=false (confirmed) creates them live, or releases the
// ones created earlier on hold.
func (c *ConfirmedOrderConsumer) handleOrderHandoff(ctx context.Context, evt *ConfirmedOrderEvent, hold bool) error {
	data := evt.Data

	orderIDStr, _ := data["order_id"].(string)
	if orderIDStr == "" {
		return fmt.Errorf("missing order_id in event data")
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

	var items []confirmedItemData
	if rawItems, ok := data["items"]; ok {
		itemBytes, _ := json.Marshal(rawItems)
		_ = json.Unmarshal(itemBytes, &items)
	}
	products := make([]confirmedItemData, 0, len(items))
	services := make([]confirmedItemData, 0)
	for _, it := range items {
		if it.isService() {
			services = append(services, it)
		} else {
			products = append(products, it)
		}
	}

	if len(services) > 0 {
		if err := c.createAppointments(ctx, tenantID, outletID, orderIDStr, data, services, hold); err != nil {
			return err
		}
	}
	if len(products) > 0 {
		if err := c.createOutletOrder(ctx, tenantID, outletID, orderIDStr, data, products, hold); err != nil {
			return err
		}
	}
	return nil
}

// releaseAccepted opens the held POS order(s) and confirms the held appointments of an online
// order that has now been accepted. Idempotent.
func (c *ConfirmedOrderConsumer) releaseAccepted(ctx context.Context, tenantID uuid.UUID, orderIDStr string) {
	links, err := c.client.OrderLink.Query().Where(entorderlink.ExternalOrderID(orderIDStr)).All(ctx)
	if err != nil {
		return
	}
	for _, l := range links {
		switch l.ChannelSource {
		case channelClickAndCollect, channelDelivery:
			c.orderSvc.ReleaseHeldOrder(ctx, tenantID, l.OrderID)
		case channelAppointment:
			_ = c.client.Appointment.UpdateOneID(l.OrderID).
				Where(entappointment.StatusEQ(entappointment.StatusScheduled)).
				SetStatus(entappointment.StatusConfirmed).
				Exec(ctx)
		}
	}
}

// createOutletOrder creates (or finds) the POS record the kitchen and counter work from.
func (c *ConfirmedOrderConsumer) createOutletOrder(ctx context.Context, tenantID, outletID uuid.UUID, orderIDStr string, data map[string]interface{}, items []confirmedItemData, hold bool) error {
	fulfillmentType, _ := data["fulfillment_type"].(string)
	source, subtype, channelSource := fulfillmentRouting(fulfillmentType)

	// Already linked: a redelivery, or the order was offered for acceptance earlier. A confirmation
	// releases the held record to the kitchen; anything else is a duplicate.
	if exists, _ := c.client.OrderLink.Query().
		Where(
			entorderlink.ExternalOrderID(orderIDStr),
			entorderlink.ChannelSourceIn(channelClickAndCollect, channelDelivery),
		).
		Exist(ctx); exists {
		if !hold {
			c.releaseAccepted(ctx, tenantID, orderIDStr)
		}
		return nil
	}

	orderNumber, _ := data["order_number"].(string)
	posOrderNumber := ""
	if orderNumber != "" {
		prefix := "CC-"
		if subtype == "delivery" {
			prefix = "DL-"
		}
		posOrderNumber = prefix + orderNumber
	}

	lines := make([]OrderLineInput, 0, len(items))
	for _, item := range items {
		// POSOrderLine.name has a NotEmpty validator; no single upstream bug should be able to
		// wedge an order out of the queue permanently, so fall back to a placeholder.
		name := item.Name
		if name == "" {
			name = "Item"
		}
		total := item.TotalPrice
		if total == 0 {
			total = item.UnitPrice * item.Quantity
		}
		lineMeta := map[string]any{}
		if len(item.Modifiers) > 0 {
			lineMeta["modifiers"] = item.Modifiers
		}
		if strings.TrimSpace(item.Notes) != "" {
			lineMeta["notes"] = strings.TrimSpace(item.Notes)
		}
		lines = append(lines, OrderLineInput{
			CatalogItemID: uuid.Nil, // online items carry no local catalog mapping
			SKU:           item.SKU,
			Name:          name,
			Category:      item.Category, // drives KDS station routing, same as a till sale
			Quantity:      item.Quantity,
			UnitPrice:     item.UnitPrice,
			TotalPrice:    total,
			TaxStatus:     "taxable",
			// Online prices are what the customer sees and pays, VAT included.
			PriceIncludesTax: true,
			Metadata:         lineMeta,
		})
	}

	meta := onlineOrderMetadata(data, orderIDStr, source, subtype, fulfillmentType, c.orderSvc.tenantLocation(ctx, tenantID))

	charges := map[string]float64{}
	if fee := numberField(data, "delivery_fee"); fee > 0 {
		charges["shipping"] = fee
	}
	customerName, _ := data["customer_name"].(string)
	customerPhone, _ := data["customer_phone"].(string)
	tenantSlug, _ := data["tenant_slug"].(string)

	order, err := c.orderSvc.CreateOrder(ctx, CreateOrderRequest{
		TenantID:          tenantID,
		TenantSlug:        tenantSlug,
		OutletID:          outletID,
		DeviceID:          onlineOrderSystemID,
		UserID:            onlineOrderSystemID,
		OrderNumber:       posOrderNumber,
		ClientReference:   onlineOrderClientReference(orderIDStr),
		Currency:          stringField(data, "currency"),
		Lines:             lines,
		Metadata:          meta,
		OrderSubtype:      subtype,
		CustomerPhone:     customerPhone,
		CustomerName:      customerName,
		DiscountAmount:    numberField(data, "discount_total"),
		Charges:           charges,
		Source:            "online_ordering",
		SkipAutoDiscounts: true,
		HoldForAcceptance: hold,
	})
	if err != nil {
		return fmt.Errorf("create POS order for online order %s: %w", orderIDStr, err)
	}

	// OrderLink records the online->POS mapping; ordering-backend's lifecycle events (KDS ready,
	// collected, cancelled) are matched through it.
	if _, err := c.client.OrderLink.Create().
		SetOrderID(order.ID).
		SetExternalOrderID(orderIDStr).
		SetChannelSource(channelSource).
		Save(ctx); err != nil {
		return fmt.Errorf("create order link: %w", err)
	}

	c.logger.Info("confirmed online order ingested into POS",
		zap.String("pos_order_id", order.ID.String()),
		zap.String("online_order_id", orderIDStr),
		zap.String("order_number", order.OrderNumber),
		zap.String("fulfillment_type", fulfillmentType),
		zap.String("source", source),
	)
	return nil
}

// onlineOrderMetadata builds the POS order metadata the online-orders queue, KDS and receipts
// read: the channel, who the customer is, whether the order is already paid (and if not, how
// much to collect on handover), the delivery destination, the order notes and the promised time.
func onlineOrderMetadata(data map[string]interface{}, orderIDStr, source, subtype, fulfillmentType string, loc *time.Location) map[string]any {
	paymentMethod := stringField(data, "payment_method")
	paymentStatus := stringField(data, "payment_status")
	grandTotal := numberField(data, "grand_total")
	prepaid := paymentStatus == "paid"
	amountDue := 0.0
	if !prepaid {
		amountDue = grandTotal
	}
	meta := map[string]any{
		"source":             source,
		"order_subtype":      subtype,
		"fulfillment_type":   fulfillmentType,
		"online_order_id":    orderIDStr,
		"online_order_no":    stringField(data, "order_number"),
		"customer_name":      stringField(data, "customer_name"),
		"customer_email":     stringField(data, "customer_email"),
		"customer_phone":     stringField(data, "customer_phone"),
		"payment_method":     paymentMethod,
		"payment_status":     paymentStatus,
		"prepaid":            prepaid,
		"amount_due":         amountDue,
		"online_grand_total": grandTotal,
	}
	// Manual M-Pesa: the customer paid the business Till/Paybill and keyed in the code at checkout.
	// The counter must verify it against the M-Pesa message before handing the order over.
	if channel := stringField(data, "payment_channel"); channel != "" {
		meta["payment_channel"] = channel
	}
	if code := stringField(data, "mpesa_code"); code != "" {
		meta["mpesa_code"] = code
	}
	if addr := stringField(data, "delivery_address"); addr != "" {
		meta["delivery_address"] = addr
	}
	if notes := stringField(data, "instructions"); notes != "" {
		meta["order_notes"] = notes
	}
	if raw := stringField(data, "scheduled_for"); raw != "" {
		if at, err := time.Parse(time.RFC3339, raw); err == nil {
			meta["scheduled_for"] = at.UTC().Format(time.RFC3339)
			if loc == nil {
				loc = time.UTC
			}
			meta["scheduled_for_label"] = at.In(loc).Format("Mon 15:04")
		}
	}
	return meta
}

// createAppointments books each service line into the outlet's appointment calendar so the salon,
// barber or garage sees the online booking next to walk-ins and phone bookings instead of a
// takeaway ticket. The deposit paid online (if any) and the balance due are noted on it.
func (c *ConfirmedOrderConsumer) createAppointments(ctx context.Context, tenantID, outletID uuid.UUID, orderIDStr string, data map[string]interface{}, services []confirmedItemData, hold bool) error {
	if exists, _ := c.client.OrderLink.Query().
		Where(entorderlink.ExternalOrderID(orderIDStr), entorderlink.ChannelSource(channelAppointment)).
		Exist(ctx); exists {
		if !hold {
			c.releaseAccepted(ctx, tenantID, orderIDStr)
		}
		return nil
	}
	// A booking waiting for the outlet to accept it shows as "scheduled" (tentative) and becomes
	// "confirmed" on acceptance.
	status := "confirmed"
	if hold {
		status = "scheduled"
	}
	loc := c.orderSvc.tenantLocation(ctx, tenantID)
	customerName := stringField(data, "customer_name")
	customerPhone := stringField(data, "customer_phone")
	orderNumber := stringField(data, "order_number")
	paid := stringField(data, "payment_status") == "paid"

	tx, err := c.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	for _, svc := range services {
		start, end := appointmentWindow(svc, loc)
		notes := appointmentNotes(orderNumber, svc, paid, stringField(data, "instructions"))
		create := tx.Appointment.Create().
			SetTenantID(tenantID).
			SetOutletID(outletID).
			SetServiceSku(firstNonEmpty(svc.SKU, "SERVICE")).
			SetStartTime(start).
			SetEndTime(end).
			SetStatus(entappointment.Status(status)).
			SetCustomerName(customerName).
			SetCustomerPhone(customerPhone).
			SetNotes(notes)
		if id, perr := uuid.Parse(stringField(svc.Metadata, "inventory_item_id")); perr == nil {
			create = create.SetServiceItemID(id)
		} else {
			create = create.SetServiceItemID(uuid.Nil)
		}
		if staff, perr := uuid.Parse(stringField(svc.Metadata, "staff_id")); perr == nil {
			create = create.SetStaffMemberID(staff)
		}
		appt, cErr := create.Save(ctx)
		if cErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("create appointment for online order %s: %w", orderIDStr, cErr)
		}
		if _, lErr := tx.OrderLink.Create().
			SetOrderID(appt.ID).
			SetExternalOrderID(orderIDStr).
			SetChannelSource(channelAppointment).
			Save(ctx); lErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("link appointment: %w", lErr)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit appointments: %w", err)
	}
	c.logger.Info("online service booking added to the appointment calendar",
		zap.String("online_order_id", orderIDStr), zap.Int("appointments", len(services)))
	return nil
}

// defaultServiceMinutes is the slot length used when neither the booking nor the catalog carries a
// service duration.
const defaultServiceMinutes = 60

// appointmentWindow resolves a service line's booked start/end in the tenant's timezone from the
// storefront's appointment_date (YYYY-MM-DD) and appointment_time (HH:mm). A line without a usable
// date/time is booked "now" so it still shows on today's calendar for staff to reschedule.
func appointmentWindow(svc confirmedItemData, loc *time.Location) (time.Time, time.Time) {
	if loc == nil {
		loc = time.UTC
	}
	date := stringField(svc.Metadata, "appointment_date")
	clock := stringField(svc.Metadata, "appointment_time")
	start, err := time.ParseInLocation("2006-01-02 15:04", date+" "+clock, loc)
	if err != nil {
		start = time.Now().In(loc).Truncate(time.Minute)
	}
	minutes := int(numberField(svc.Metadata, "duration_minutes"))
	if minutes <= 0 {
		minutes = defaultServiceMinutes
	}
	qty := int(svc.Quantity)
	if qty > 1 {
		minutes *= qty
	}
	return start, start.Add(time.Duration(minutes) * time.Minute)
}

// appointmentNotes summarises the online booking for the staff calendar.
func appointmentNotes(orderNumber string, svc confirmedItemData, paid bool, customerNotes string) string {
	parts := []string{fmt.Sprintf("Online booking %s: %s", orderNumber, svc.Name)}
	if paid {
		parts = append(parts, "paid online")
	} else {
		parts = append(parts, "pay at the appointment")
	}
	if pct := numberField(svc.Metadata, "deposit_percent"); pct > 0 {
		parts = append(parts, fmt.Sprintf("deposit %.0f%% collected online, balance due at the appointment", pct))
	}
	if n := strings.TrimSpace(firstNonEmpty(svc.Notes, customerNotes)); n != "" {
		parts = append(parts, "notes: "+n)
	}
	return strings.Join(parts, "; ")
}

func stringField(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func numberField(m map[string]interface{}, key string) float64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
