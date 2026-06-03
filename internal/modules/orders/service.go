// Package orders provides the order service layer for POS operations.
// It encapsulates business logic for order creation, tax/discount calculation,
// and order lifecycle management that was previously hardcoded in handlers.
package orders

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/kdsstation"
	"github.com/bengobox/pos-service/internal/ent/kdsticket"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	kdsmod "github.com/bengobox/pos-service/internal/modules/kds"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// OrderStatus defines valid order states.
const (
	StatusDraft          = "draft"
	StatusOpen           = "open"
	StatusPendingPayment = "pending_payment" // all KDS tickets served; awaiting cashier payment
	StatusCompleted      = "completed"
	StatusCancelled      = "cancelled"
	StatusRefunded       = "refunded"
	StatusVoided         = "voided"
)

// validTransitions defines allowed status transitions.
var validTransitions = map[string][]string{
	// draft → completed is required for retail orders that skip the "open" stage.
	StatusDraft:          {StatusOpen, StatusPendingPayment, StatusCompleted, StatusCancelled, StatusVoided},
	StatusOpen:           {StatusPendingPayment, StatusCompleted, StatusCancelled, StatusVoided},
	StatusPendingPayment: {StatusCompleted, StatusCancelled, StatusVoided},
	StatusCompleted:      {StatusRefunded},
	StatusCancelled:      {},
	StatusRefunded:       {},
	StatusVoided:         {},
}

// CreateOrderRequest holds the input for creating a POS order.
type CreateOrderRequest struct {
	TenantID       uuid.UUID
	TenantSlug     string // used for treasury S2S tax lookups
	OutletID       uuid.UUID
	DeviceID       uuid.UUID
	UserID         uuid.UUID
	OrderNumber    string
	Currency       string
	Lines          []OrderLineInput
	Metadata       map[string]any
	OrderSubtype   string // dine_in | takeaway | room_service | delivery | bar_tab | retail; defaults to "dine_in"
	TableID        string // UUID of the table (hospitality dine-in); stored in metadata (no DB column yet)
	CustomerPhone  string // loyalty auto-earn — stored on order, forwarded in pos.sale.finalized
	CustomerName   string
	DiscountAmount float64 // order-level discount (e.g. loyalty redemption) applied before total_amount
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
	// happyHourFn evaluates the best auto-apply (happy-hour / negotiated-meal) discount for
	// (tenant, outlet) on the given lines at the current time, enforcing rule scope. Returns the
	// winning promo id (uuid.Nil if none) and the discount amount. Injected from the promotions
	// service to keep packages decoupled.
	happyHourFn func(ctx context.Context, tenantID, outletID uuid.UUID, lines []OrderLineInput) (uuid.UUID, decimal.Decimal)
	// recordPromoFn writes the PromotionApplication audit row once the order id is known.
	recordPromoFn func(ctx context.Context, promoID, orderID uuid.UUID, amount decimal.Decimal)
}

// SetPublisher sets the event publisher for order lifecycle events.
func (s *Service) SetPublisher(p *events.Publisher) {
	s.publisher = p
}

// SetHappyHourEvaluator wires the auto-apply discount evaluator + audit recorder (from promotions service).
func (s *Service) SetHappyHourEvaluator(
	fn func(ctx context.Context, tenantID, outletID uuid.UUID, lines []OrderLineInput) (uuid.UUID, decimal.Decimal),
	record func(ctx context.Context, promoID, orderID uuid.UUID, amount decimal.Decimal),
) {
	s.happyHourFn = fn
	s.recordPromoFn = record
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

	discount := decimal.NewFromFloat(req.DiscountAmount)
	// Auto-apply the best happy-hour / negotiated-meal discount (scope-enforced) on top of any
	// explicit discount. Capture the winning promo so we can write the audit row after save.
	var appliedPromoID uuid.UUID
	var appliedPromoAmount decimal.Decimal
	if s.happyHourFn != nil {
		if promoID, hh := s.happyHourFn(ctx, req.TenantID, req.OutletID, req.Lines); hh.IsPositive() {
			discount = discount.Add(hh)
			appliedPromoID = promoID
			appliedPromoAmount = hh
			if meta := req.Metadata; meta != nil {
				meta["happy_hour_discount"] = hh.InexactFloat64()
			}
		}
	}
	totals := s.CalculateTotals(req.Lines, discount)

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

	// Hospitality order subtypes are opened immediately so the kitchen
	// receives a KDS ticket as soon as the waiter places the order.
	initialStatus := StatusDraft
	isHospitalityOrder := subtype == "dine_in" || subtype == "takeaway" || subtype == "room_service" || subtype == "bar_tab"
	if isHospitalityOrder {
		initialStatus = StatusOpen
	}

	orderBuilder := tx.POSOrder.Create().
		SetTenantID(req.TenantID).
		SetOutletID(req.OutletID).
		SetDeviceID(req.DeviceID).
		SetUserID(req.UserID).
		SetOrderNumber(orderNumber).
		SetStatus(initialStatus).
		SetSubtotal(totals.Subtotal.InexactFloat64()).
		SetTaxTotal(totals.TaxTotal.InexactFloat64()).
		SetDiscountTotal(totals.DiscountTotal.InexactFloat64()).
		SetTotalAmount(totals.TotalAmount.InexactFloat64()).
		SetCurrency(currency).
		SetOrderSubtype(posorder.OrderSubtype(subtype)).
		SetMetadata(meta)
	if req.CustomerPhone != "" {
		orderBuilder = orderBuilder.SetCustomerPhone(req.CustomerPhone)
	}
	if req.CustomerName != "" {
		orderBuilder = orderBuilder.SetCustomerName(req.CustomerName)
	}
	order, err := orderBuilder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("orders: create order: %w", err)
	}

	// Batch-resolve KDS station IDs from catalog overrides for all line SKUs.
	// This is the primary routing mechanism: managers assign items to stations in POS settings.
	skus := make([]string, 0, len(req.Lines))
	for _, l := range req.Lines {
		if l.SKU != "" {
			skus = append(skus, l.SKU)
		}
	}
	kdsOverrideBySKU := make(map[string]uuid.UUID)
	if len(skus) > 0 {
		overrides, _ := s.client.POSCatalogOverride.Query().
			Where(
				entoverride.TenantID(req.TenantID),
				entoverride.InventorySkuIn(skus...),
				entoverride.KdsStationIDNotNil(),
			).All(ctx)
		for _, o := range overrides {
			if o.KdsStationID != nil {
				kdsOverrideBySKU[o.InventorySku] = *o.KdsStationID
			}
		}
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
		// Stamp the KDS station on the line so routing in createKDSTicketsForOrder is O(1).
		if stationID, ok := kdsOverrideBySKU[line.SKU]; ok {
			lineCreate = lineCreate.SetKdsStationID(stationID)
		}

		_, err = lineCreate.Save(ctx)
		if err != nil {
			return nil, fmt.Errorf("orders: create line: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("orders: commit: %w", err)
	}

	// Audit the applied auto-discount (PromotionApplication) now that the order id exists.
	if s.recordPromoFn != nil && appliedPromoID != uuid.Nil && appliedPromoAmount.IsPositive() {
		s.recordPromoFn(ctx, appliedPromoID, order.ID, appliedPromoAmount)
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

	// For hospitality orders that were auto-opened, create KDS tickets immediately.
	if isHospitalityOrder {
		_ = s.createKDSTicketsForOrder(ctx, req.TenantID, result)
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

// parseTableRef extracts the table reference string from an order's metadata.
func parseTableRef(order *ent.POSOrder) string {
	if v, ok := order.Metadata["table_number"].(string); ok && v != "" {
		return v
	}
	if v, ok := order.Metadata["table_name"].(string); ok && v != "" {
		return v
	}
	return ""
}

// routeLinesToStations groups order lines into per-station item buckets.
//
// Routing priority (highest to lowest):
//  1. line.KdsStationID — explicit station set at order creation from POSCatalogOverride
//  2. Station category_filter — keyword match against the item name (fallback)
//  3. Expo / "all" stations — receive every item as a secondary copy for the expediter
//
// A station with station_type="expo" or "all" always receives EVERY item.
// Items with no explicit station and no matching category_filter go to expo/all stations;
// if no such station exists they go to the first active station.
func routeLinesToStations(lines []*ent.POSOrderLine, stations []*ent.KDSStation) map[uuid.UUID][]map[string]any {
	stationItems := make(map[uuid.UUID][]map[string]any, len(stations))

	// Identify expo/all stations upfront.
	var expoIDs []uuid.UUID
	for _, st := range stations {
		if st.StationType == "expo" || st.StationType == "all" {
			expoIDs = append(expoIDs, st.ID)
		}
	}

	for _, l := range lines {
		item := map[string]any{
			"sku":      l.Sku,
			"name":     l.Name,
			"quantity": l.Quantity,
		}

		routed := false

		// Priority 1: explicit station on the order line (set from catalog override).
		if l.KdsStationID != nil {
			stationItems[*l.KdsStationID] = append(stationItems[*l.KdsStationID], item)
			routed = true
		}

		// Priority 2: category_filter keyword match on item name (case-insensitive).
		if !routed {
			nameLower := strings.ToLower(l.Name)
			for _, st := range stations {
				if st.StationType == "expo" || st.StationType == "all" {
					continue // handled separately below
				}
				for _, cat := range st.CategoryFilter {
					if strings.Contains(nameLower, strings.ToLower(cat)) {
						stationItems[st.ID] = append(stationItems[st.ID], item)
						routed = true
						break
					}
				}
				if routed {
					break
				}
			}
		}

		// Priority 3: no specific station matched — route to expo/all as the catch-all,
		// or fall back to the first active station. Expo only receives items that are
		// genuinely unresolved (no kitchen, bar, or other station matched them).
		if !routed {
			if len(expoIDs) > 0 {
				for _, eid := range expoIDs {
					stationItems[eid] = append(stationItems[eid], item)
				}
			} else if len(stations) > 0 {
				stationItems[stations[0].ID] = append(stationItems[stations[0].ID], item)
			}
		}
	}

	return stationItems
}

// createKDSTicketsForOrder creates per-station KDS tickets with only the items
// relevant to each station. Items are routed via kds_station_id on the order line
// (resolved from POSCatalogOverride at order creation) with a category_filter
// keyword fallback. Expo/all stations receive every item as a secondary copy.
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

	stationItems := routeLinesToStations(lines, stations)
	tableRef := parseTableRef(order)

	for _, station := range stations {
		items := stationItems[station.ID]
		if len(items) == 0 {
			continue // no items for this station — skip
		}
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

// AddOrderLines appends new lines to an existing open order, recalculates totals,
// and creates KDS tickets for the new course_number=0 lines (always-fire items).
func (s *Service) AddOrderLines(ctx context.Context, tenantID, orderID uuid.UUID, lines []OrderLineInput) (*ent.POSOrder, error) {
	order, err := s.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tenantID)).
		WithLines().
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("orders: order not found: %w", err)
	}
	if order.Status != StatusOpen {
		return nil, fmt.Errorf("orders: can only add lines to open orders, current status: %s", order.Status)
	}

	// Resolve KDS station IDs from catalog overrides for new line SKUs.
	skus := make([]string, 0, len(lines))
	for _, l := range lines {
		if l.SKU != "" {
			skus = append(skus, l.SKU)
		}
	}
	kdsOverrideBySKU := make(map[string]uuid.UUID)
	if len(skus) > 0 {
		overrides, _ := s.client.POSCatalogOverride.Query().
			Where(
				entoverride.TenantID(tenantID),
				entoverride.InventorySkuIn(skus...),
				entoverride.KdsStationIDNotNil(),
			).All(ctx)
		for _, o := range overrides {
			if o.KdsStationID != nil {
				kdsOverrideBySKU[o.InventorySku] = *o.KdsStationID
			}
		}
	}

	newLines := make([]*ent.POSOrderLine, 0, len(lines))
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("orders: begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	for _, l := range lines {
		lineTotal := decimal.NewFromFloat(l.TotalPrice)
		if lineTotal.IsZero() {
			lineTotal = decimal.NewFromFloat(l.UnitPrice).Mul(decimal.NewFromFloat(l.Quantity))
		}
		meta := l.Metadata
		if meta == nil {
			meta = map[string]any{}
		}
		lc := tx.POSOrderLine.Create().
			SetOrderID(order.ID).
			SetCatalogItemID(l.CatalogItemID).
			SetSku(l.SKU).
			SetName(l.Name).
			SetQuantity(l.Quantity).
			SetUnitPrice(l.UnitPrice).
			SetTotalPrice(lineTotal.InexactFloat64()).
			SetCourseNumber(l.CourseNumber).
			SetMetadata(meta)
		if stationID, ok := kdsOverrideBySKU[l.SKU]; ok {
			lc = lc.SetKdsStationID(stationID)
		}
		saved, saveErr := lc.Save(ctx)
		if saveErr != nil {
			err = fmt.Errorf("orders: create line: %w", saveErr)
			return nil, err
		}
		newLines = append(newLines, saved)
	}

	// Recalculate totals from all lines (existing + new).
	allLines := append(order.Edges.Lines, newLines...)
	var newSubtotal, newTaxTotal decimal.Decimal
	for _, ol := range allLines {
		newSubtotal = newSubtotal.Add(decimal.NewFromFloat(ol.TotalPrice))
		if ol.TaxAmount != nil {
			newTaxTotal = newTaxTotal.Add(decimal.NewFromFloat(*ol.TaxAmount))
		}
	}
	newTotal := newSubtotal.Add(newTaxTotal)

	_, err = tx.POSOrder.UpdateOneID(order.ID).
		SetSubtotal(newSubtotal.InexactFloat64()).
		SetTaxTotal(newTaxTotal.InexactFloat64()).
		SetTotalAmount(newTotal.InexactFloat64()).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("orders: update totals: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("orders: commit: %w", err)
	}

	// Reload order with all lines.
	result, err := s.client.POSOrder.Query().
		Where(posorder.ID(order.ID)).
		WithLines().
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("orders: reload: %w", err)
	}

	// All lines added to an existing open order fire to KDS immediately.
	// Course-number gating only applies to the initial order submission — once
	// a waiter taps "Add to Bill" on a live order, the kitchen needs to know now.
	if len(newLines) > 0 {
		_ = s.createKDSTicketsForNewLines(ctx, tenantID, result, newLines)
	}

	return result, nil
}

// createKDSTicketsForNewLines creates KDS tickets for a specific subset of lines
// (used when adding items to an existing bill — always creates new tickets, never deduplicates).
func (s *Service) createKDSTicketsForNewLines(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder, newLines []*ent.POSOrderLine) error {
	stations, err := s.client.KDSStation.Query().
		Where(kdsstation.TenantID(tenantID), kdsstation.OutletID(order.OutletID), kdsstation.IsActive(true)).
		All(ctx)
	if err != nil || len(stations) == 0 {
		return nil
	}

	stationItems := routeLinesToStations(newLines, stations)
	tableRef := parseTableRef(order)

	for _, station := range stations {
		items := stationItems[station.ID]
		if len(items) == 0 {
			continue
		}
		ticket, tErr := s.client.KDSTicket.Create().
			SetTenantID(tenantID).
			SetStationID(station.ID).
			SetOrderID(order.ID).
			SetOrderNumber(order.OrderNumber).
			SetStatus(kdsticket.StatusPending).
			SetItems(items).
			SetTableReference(tableRef).
			Save(ctx)
		if tErr != nil {
			s.log.Warn("kds: add-lines ticket creation failed",
				zap.String("order_id", order.ID.String()),
				zap.Error(tErr))
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
					"status":          string(kdsticket.StatusPending),
					"items":           items,
				},
			})
		}
	}
	return nil
}

// FireCourseKDS creates KDS tickets for order lines with course_number == course,
// routing each line to the correct station based on kds_station_id / category_filter.
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

	stationItems := routeLinesToStations(courseLines, stations)
	tableRef := parseTableRef(order)

	for _, station := range stations {
		items := stationItems[station.ID]
		if len(items) == 0 {
			continue
		}
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
