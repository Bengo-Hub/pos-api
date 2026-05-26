// Package orders provides the order service layer for POS operations.
// It encapsulates business logic for order creation, tax/discount calculation,
// and order lifecycle management that was previously hardcoded in handlers.
package orders

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/kdsticket"
	"github.com/bengobox/pos-service/internal/ent/kdsstation"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	kdsmod "github.com/bengobox/pos-service/internal/modules/kds"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// OrderStatus defines valid order states.
const (
	StatusDraft      = "draft"
	StatusOpen       = "open"
	StatusCompleted  = "completed"
	StatusCancelled  = "cancelled"
	StatusRefunded   = "refunded"
	StatusVoided     = "voided"
)

// validTransitions defines allowed status transitions.
var validTransitions = map[string][]string{
	StatusDraft:     {StatusOpen, StatusCancelled, StatusVoided},
	StatusOpen:      {StatusCompleted, StatusCancelled, StatusVoided},
	StatusCompleted: {StatusRefunded},
	StatusCancelled: {},
	StatusRefunded:  {},
	StatusVoided:    {},
}

// CreateOrderRequest holds the input for creating a POS order.
type CreateOrderRequest struct {
	TenantID     uuid.UUID
	TenantSlug   string    // used for treasury S2S tax lookups
	OutletID     uuid.UUID
	DeviceID     uuid.UUID
	UserID       uuid.UUID
	OrderNumber  string
	Currency     string
	Lines        []OrderLineInput
	Metadata     map[string]any
	OrderSubtype string    // dine_in | takeaway | room_service | delivery | bar_tab; defaults to "dine_in"
	TableID      string    // UUID of the table (hospitality dine-in); stored in metadata (no DB column yet)
}

// OrderLineInput represents a single line item in an order.
type OrderLineInput struct {
	CatalogItemID    uuid.UUID
	SKU              string
	Name             string
	Quantity         float64
	UnitPrice        float64
	TotalPrice       float64
	TaxStatus        string         // "taxable", "exempt", "zero_rated"
	TaxCodeID        string         // Treasury TaxCode.code (e.g. "VAT-16"); empty = use service default
	PriceIncludesTax bool           // True if UnitPrice is VAT-inclusive
	CourseNumber     int            // 0=fire immediately, 1=Starter, 2=Main, 3=Dessert (0 = default)
	Metadata         map[string]any // modifiers, notes, serial numbers, etc.
}

// OrderTotals holds calculated totals for an order.
type OrderTotals struct {
	Subtotal      decimal.Decimal
	TaxTotal      decimal.Decimal
	DiscountTotal decimal.Decimal
	TotalAmount   decimal.Decimal
}

// Service provides order business logic.
type Service struct {
	client          *ent.Client
	log             *zap.Logger
	defaultCurrency string
	taxRate         decimal.Decimal // fallback tax rate when treasury tax code not available
	orderPrefix     string
	publisher       *events.Publisher
	taxResolver     *TaxResolver // resolves tax codes from treasury with Redis cache
	kdsHub          *kdsmod.Hub
}

// SetPublisher sets the event publisher for order lifecycle events.
func (s *Service) SetPublisher(p *events.Publisher) {
	s.publisher = p
}

// SetTaxResolver attaches the treasury tax resolver for per-line tax computation.
func (s *Service) SetTaxResolver(tr *TaxResolver) {
	s.taxResolver = tr
}

// SetKDSHub wires the KDS WebSocket hub so new tickets broadcast immediately.
func (s *Service) SetKDSHub(h *kdsmod.Hub) {
	s.kdsHub = h
}

// GetPublisher returns the event publisher (nil if not set).
func (s *Service) GetPublisher() *events.Publisher {
	return s.publisher
}

// Config holds order service configuration.
type Config struct {
	DefaultCurrency string
	TaxRatePercent  float64 // e.g. 16.0 for 16% VAT
	OrderPrefix     string
}

// NewService creates a new order service.
func NewService(client *ent.Client, cfg Config, log *zap.Logger) *Service {
	currency := cfg.DefaultCurrency
	if currency == "" {
		currency = "KES"
	}
	prefix := cfg.OrderPrefix
	if prefix == "" {
		prefix = "POS"
	}
	taxRate := decimal.NewFromFloat(cfg.TaxRatePercent).Div(decimal.NewFromInt(100))

	return &Service{
		client:          client,
		log:             log.Named("orders.service"),
		defaultCurrency: currency,
		taxRate:         taxRate,
		orderPrefix:     prefix,
	}
}

// CalculateTotals computes subtotal, tax, discount, and total for order lines.
func (s *Service) CalculateTotals(lines []OrderLineInput, discountAmount decimal.Decimal) OrderTotals {
	subtotal := decimal.Zero
	taxableAmount := decimal.Zero

	for _, line := range lines {
		lineTotal := decimal.NewFromFloat(line.TotalPrice)
		if lineTotal.IsZero() {
			lineTotal = decimal.NewFromFloat(line.UnitPrice).Mul(decimal.NewFromFloat(line.Quantity))
		}
		subtotal = subtotal.Add(lineTotal)

		if line.TaxStatus == "" || line.TaxStatus == "taxable" {
			taxableAmount = taxableAmount.Add(lineTotal)
		}
	}

	taxTotal := decimal.Zero
	if s.taxRate.IsPositive() {
		taxTotal = taxableAmount.Mul(s.taxRate).Round(2)
	}

	if discountAmount.IsNegative() {
		discountAmount = decimal.Zero
	}

	totalAmount := subtotal.Add(taxTotal).Sub(discountAmount)
	if totalAmount.IsNegative() {
		totalAmount = decimal.Zero
	}

	return OrderTotals{
		Subtotal:      subtotal.Round(2),
		TaxTotal:      taxTotal.Round(2),
		DiscountTotal: discountAmount.Round(2),
		TotalAmount:   totalAmount.Round(2),
	}
}

// GenerateOrderNumber creates a unique order number.
func (s *Service) GenerateOrderNumber() string {
	return fmt.Sprintf("%s-%d", s.orderPrefix, time.Now().UnixMilli())
}

// DefaultCurrency returns the configured default currency.
func (s *Service) DefaultCurrency() string {
	return s.defaultCurrency
}

// CreateOrder creates a new POS order with proper tax/discount calculation.
func (s *Service) CreateOrder(ctx context.Context, req CreateOrderRequest) (*ent.POSOrder, error) {
	currency := req.Currency
	if currency == "" {
		currency = s.defaultCurrency
	}
	orderNumber := req.OrderNumber
	if orderNumber == "" {
		orderNumber = s.GenerateOrderNumber()
	}

	totals := s.CalculateTotals(req.Lines, decimal.Zero)

	// Resolve order subtype, defaulting to dine_in.
	subtype := req.OrderSubtype
	if subtype == "" {
		subtype = "dine_in"
	}

	// Carry table_id in metadata (no dedicated DB column yet).
	meta := req.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	if req.TableID != "" {
		meta["table_id"] = req.TableID
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("orders: begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	order, err := tx.POSOrder.Create().
		SetTenantID(req.TenantID).
		SetOutletID(req.OutletID).
		SetDeviceID(req.DeviceID).
		SetUserID(req.UserID).
		SetOrderNumber(orderNumber).
		SetStatus(StatusDraft).
		SetSubtotal(totals.Subtotal.InexactFloat64()).
		SetTaxTotal(totals.TaxTotal.InexactFloat64()).
		SetDiscountTotal(totals.DiscountTotal.InexactFloat64()).
		SetTotalAmount(totals.TotalAmount.InexactFloat64()).
		SetCurrency(currency).
		SetOrderSubtype(posorder.OrderSubtype(subtype)).
		SetMetadata(meta).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("orders: create order: %w", err)
	}

	for _, line := range req.Lines {
		lineTotal := decimal.NewFromFloat(line.TotalPrice)
		if lineTotal.IsZero() {
			lineTotal = decimal.NewFromFloat(line.UnitPrice).Mul(decimal.NewFromFloat(line.Quantity))
		}
		meta := line.Metadata
		if meta == nil {
			meta = map[string]any{}
		}

		// Resolve tax for this line via treasury S2S (Redis-cached).
		// Idempotency: if caller provided explicit TaxCodeID, use it;
		// otherwise skip tax (treasury is the source of truth; not all items are taxable).
		var taxKraCode, taxCodeID string
		var taxRate, taxAmt float64
		priceIncludesTax := line.PriceIncludesTax

		if line.TaxStatus != "tax_exempt" && line.TaxStatus != "zero_rated" && s.taxResolver != nil && line.TaxCodeID != "" {
			taxCodeID = line.TaxCodeID
			if tc, resolveErr := s.taxResolver.Resolve(ctx, req.TenantSlug, line.TaxCodeID); resolveErr == nil && tc != nil {
				taxRate = tc.Rate
				taxKraCode = tc.KRACode
				taxAmt, _ = ComputeLineTax(lineTotal.InexactFloat64(), taxRate, priceIncludesTax)
			}
		}

		lineCreate := tx.POSOrderLine.Create().
			SetOrderID(order.ID).
			SetCatalogItemID(line.CatalogItemID).
			SetSku(line.SKU).
			SetName(line.Name).
			SetQuantity(line.Quantity).
			SetUnitPrice(line.UnitPrice).
			SetTotalPrice(lineTotal.InexactFloat64()).
			SetPriceIncludesTax(priceIncludesTax).
			SetCourseNumber(line.CourseNumber).
			SetMetadata(meta)

		if taxCodeID != "" {
			lineCreate = lineCreate.SetTaxCodeID(taxCodeID)
		}
		if taxKraCode != "" {
			lineCreate = lineCreate.SetTaxKraCode(taxKraCode)
		}
		if taxRate > 0 {
			lineCreate = lineCreate.SetTaxRate(taxRate)
		}
		if taxAmt > 0 {
			lineCreate = lineCreate.SetTaxAmount(taxAmt)
		}

		_, err = lineCreate.Save(ctx)
		if err != nil {
			return nil, fmt.Errorf("orders: create line: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("orders: commit: %w", err)
	}

	// Re-query with edges loaded
	result, err := s.client.POSOrder.Query().
		Where(posorder.ID(order.ID)).
		WithLines().
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("orders: reload: %w", err)
	}

	// Publish order created event
	if s.publisher != nil {
		_ = s.publisher.PublishOrderCreated(ctx, req.TenantID, map[string]any{
			"order_id":     order.ID.String(),
			"order_number": orderNumber,
			"outlet_id":    req.OutletID.String(),
			"total_amount": totals.TotalAmount.String(),
			"currency":     currency,
			"item_count":   len(req.Lines),
		})
	}

	return result, nil
}

// ValidateStatusTransition checks if a status transition is allowed.
func (s *Service) ValidateStatusTransition(current, next string) error {
	allowed, ok := validTransitions[current]
	if !ok {
		return fmt.Errorf("orders: unknown current status %q", current)
	}
	for _, a := range allowed {
		if a == next {
			return nil
		}
	}
	return fmt.Errorf("orders: invalid transition from %q to %q", current, next)
}

// UpdateStatus transitions an order to a new status with validation.
func (s *Service) UpdateStatus(ctx context.Context, tenantID, orderID uuid.UUID, newStatus string) (*ent.POSOrder, error) {
	order, err := s.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("orders: not found: %w", err)
	}

	if err := s.ValidateStatusTransition(order.Status, newStatus); err != nil {
		return nil, err
	}

	updated, err := order.Update().SetStatus(newStatus).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("orders: update status: %w", err)
	}

	// Publish order status changed event
	if s.publisher != nil {
		_ = s.publisher.PublishOrderStatusChanged(ctx, tenantID, map[string]any{
			"order_id":        orderID.String(),
			"order_number":    order.OrderNumber,
			"previous_status": order.Status,
			"new_status":      newStatus,
		})
	}

	// Create KDS tickets when a POS-native order is opened (sent to kitchen)
	if newStatus == StatusOpen {
		_ = s.createKDSTicketsForOrder(ctx, tenantID, updated)
	}

	return updated, nil
}

// createKDSTicketsForOrder creates KDS tickets for all active stations when a POS order opens.
func (s *Service) createKDSTicketsForOrder(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder) error {
	stations, err := s.client.KDSStation.Query().
		Where(kdsstation.TenantID(tenantID), kdsstation.OutletID(order.OutletID), kdsstation.IsActive(true)).
		All(ctx)
	if err != nil || len(stations) == 0 {
		return nil
	}

	lines, err := s.client.POSOrderLine.Query().
		Where(posorderline.OrderID(order.ID)).
		All(ctx)
	if err != nil {
		return err
	}

	items := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		items = append(items, map[string]any{
			"sku":      l.Sku,
			"name":     l.Name,
			"quantity": l.Quantity,
		})
	}

	// Parse table_reference from order metadata (set by hospitality terminal when table is assigned).
	tableRef := ""
	if v, ok := order.Metadata["table_number"]; ok {
		if s, ok := v.(string); ok {
			tableRef = s
		}
	}
	if tableRef == "" {
		if v, ok := order.Metadata["table_name"]; ok {
			if s, ok := v.(string); ok {
				tableRef = s
			}
		}
	}

	for _, station := range stations {
		exists, _ := s.client.KDSTicket.Query().
			Where(kdsticket.OrderID(order.ID), kdsticket.StationID(station.ID)).
			Exist(ctx)
		if exists {
			continue
		}
		cc := s.client.KDSTicket.Create().
			SetTenantID(tenantID).
			SetStationID(station.ID).
			SetOrderID(order.ID).
			SetOrderNumber(order.OrderNumber).
			SetStatus(kdsticket.StatusPending).
			SetItems(items)
		if tableRef != "" {
			cc = cc.SetTableReference(tableRef)
		}
		ticket, err := cc.Save(ctx)
		if err != nil {
			s.log.Warn("kds: failed to create ticket for pos order",
				zap.String("order_id", order.ID.String()),
				zap.String("station_id", station.ID.String()),
				zap.Error(err))
			continue
		}
		if s.kdsHub != nil {
			s.kdsHub.BroadcastToOutlet(order.TenantID, order.OutletID, kdsmod.Message{
				Type: "ticket_created",
				Payload: map[string]any{
					"ticket_id":       ticket.ID,
					"order_id":        order.ID,
					"order_number":    order.OrderNumber,
					"station_id":      station.ID,
					"table_reference": tableRef,
					"status":          string(kdsticket.StatusPending),
					"items":           items,
				},
			})
		}
	}
	return nil
}

// FireCourseKDS creates KDS tickets for order lines with course_number == course.
// The order must be queried with WithLines() before calling.
func (s *Service) FireCourseKDS(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder, course int) error {
	courseLines := make([]*ent.POSOrderLine, 0)
	for _, l := range order.Edges.Lines {
		if l.CourseNumber == course {
			courseLines = append(courseLines, l)
		}
	}
	if len(courseLines) == 0 {
		return nil
	}

	stations, err := s.client.KDSStation.Query().
		Where(kdsstation.TenantID(tenantID), kdsstation.OutletID(order.OutletID), kdsstation.IsActive(true)).
		All(ctx)
	if err != nil || len(stations) == 0 {
		return err
	}

	items := make([]map[string]any, 0, len(courseLines))
	for _, l := range courseLines {
		items = append(items, map[string]any{
			"sku":      l.Sku,
			"name":     l.Name,
			"quantity": l.Quantity,
		})
	}

	tableRef := ""
	if v, ok := order.Metadata["table_number"].(string); ok {
		tableRef = v
	}

	for _, station := range stations {
		ticket, err := s.client.KDSTicket.Create().
			SetTenantID(tenantID).
			SetStationID(station.ID).
			SetOrderID(order.ID).
			SetOrderNumber(order.OrderNumber).
			SetStatus(kdsticket.StatusPending).
			SetItems(items).
			SetTableReference(tableRef).
			Save(ctx)
		if err != nil {
			s.log.Warn("kds: fire-course ticket creation failed",
				zap.String("order_id", order.ID.String()),
				zap.Int("course", course),
				zap.Error(err))
			continue
		}
		if s.kdsHub != nil {
			s.kdsHub.BroadcastToOutlet(tenantID, order.OutletID, kdsmod.Message{
				Type: "ticket_created",
				Payload: map[string]any{
					"ticket_id":       ticket.ID,
					"order_id":        order.ID,
					"order_number":    order.OrderNumber,
					"station_id":      station.ID,
					"table_reference": tableRef,
					"course":          course,
					"status":          string(kdsticket.StatusPending),
					"items":           items,
				},
			})
		}
	}
	return nil
}
