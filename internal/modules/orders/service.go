// Package orders provides the order service layer for POS operations.
// It encapsulates business logic for order creation, tax/discount calculation,
// and order lifecycle management that was previously hardcoded in handlers.
package orders

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/kdsstation"
	"github.com/bengobox/pos-service/internal/ent/kdsticket"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	kdsmod "github.com/bengobox/pos-service/internal/modules/kds"
	"github.com/bengobox/pos-service/internal/modules/printing"
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

// lineIsNonBillable reports whether an order line is flagged free-of-charge: the POS
// catalog marks non-billable items (inventory Item.non_billable) and complimentary
// accompaniments, and the till carries the flag in the line metadata.
func lineIsNonBillable(meta map[string]any) bool {
	if meta == nil {
		return false
	}
	for _, key := range []string{"non_billable", "complimentary", "is_complimentary"} {
		switch v := meta[key].(type) {
		case bool:
			if v {
				return true
			}
		case string:
			if strings.EqualFold(v, "true") {
				return true
			}
		}
	}
	return false
}

// ErrInvalidOrderSubtype is returned when an order create carries an order_subtype outside
// the schema enum. Handlers map it to a 400 (it used to surface as an opaque 500).
var ErrInvalidOrderSubtype = errors.New("invalid order_subtype")

// validOrderSubtypes mirrors the posorder.OrderSubtype enum values. "draft" is deliberately
// absent — it is an order STATUS, not a subtype (legacy clients send it from Save as Draft).
var validOrderSubtypes = map[string]struct{}{
	"dine_in": {}, "takeaway": {}, "room_service": {}, "delivery": {}, "bar_tab": {}, "retail": {},
}

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
	// ClientReference is the offline device's local id (uuid). When set, CreateOrder is
	// get-or-create on it, making replayed offline sales idempotent.
	ClientReference string
	// OfflineCreatedAt is the device-clock time the sale was rung up (offline). Optional.
	OfflineCreatedAt *time.Time
	Currency         string
	Lines          []OrderLineInput
	Metadata       map[string]any
	OrderSubtype   string // dine_in | takeaway | room_service | delivery | bar_tab | retail; defaults to "dine_in"
	TableID        string // UUID of the table (hospitality dine-in); stored in metadata (no DB column yet)
	CustomerPhone  string // loyalty auto-earn — stored on order, forwarded in pos.sale.finalized
	CustomerName   string
	DiscountAmount float64 // order-level discount (e.g. loyalty redemption) applied before total_amount
	// OrderTaxAmount is a manager/admin order-level tax adjustment ADDED on top of the per-line
	// tax (quick-edit "Edit Order Tax"). Folds into tax_total; the edit is recorded in
	// metadata.order_tax for receipts/audit.
	OrderTaxAmount float64
	// Charges are additional order-level costs (keys: packaging, service, shipping) that
	// increase total_amount. Sum lands in charges_total; breakdown in metadata.charges.
	Charges map[string]float64
	// Source marks where the sale originated: "pos_terminal" (default) or "back_office"
	// (the back-office Add Sale flow). Drives the All-Sales Sources filter + POS-only list.
	Source string
}

// OrderLineInput represents a single line item in an order.
type OrderLineInput struct {
	CatalogItemID    uuid.UUID
	SKU              string
	Name             string
	Category         string // Item category name; drives KDS station routing (kitchen vs bar)
	Quantity         float64
	UnitPrice        float64
	TotalPrice       float64
	TaxStatus        string         // "taxable", "exempt", "zero_rated"
	TaxCodeID        string         // Treasury TaxCode.code (e.g. "VAT-16"); empty = use service default
	PriceIncludesTax bool           // True if UnitPrice is VAT-inclusive
	TaxRate          *float64       // VAT % the till applied (treasury-enriched catalog); nil = not provided
	CourseNumber     int            // 0=fire immediately, 1=Starter, 2=Main, 3=Dessert (0 = default)
	Metadata         map[string]any // modifiers, notes, serial numbers, etc.
}

// OrderTotals holds calculated totals for an order. The identity
// TotalAmount = Subtotal + TaxTotal - DiscountTotal + ChargesTotal + RoundOff always holds,
// with TotalAmount a whole number (ceiling round-off, QA: "no decimal points on totals").
type OrderTotals struct {
	Subtotal      decimal.Decimal
	TaxTotal      decimal.Decimal
	DiscountTotal decimal.Decimal
	ChargesTotal  decimal.Decimal
	RoundOff      decimal.Decimal
	TotalAmount   decimal.Decimal
}

// finalizeTotals is the single choke point that turns raw components into the stored totals:
// it clamps the discount, adds order-level tax + charges, and rounds the payable UP to the
// next whole number with the difference recorded as RoundOff — mirroring the till's
// applyRoundOff (pos-ui src/lib/pos/cart-tax.ts) so server total == till total.
func finalizeTotals(subtotal, taxTotal, discount, chargesTotal, orderTax decimal.Decimal) OrderTotals {
	if discount.IsNegative() {
		discount = decimal.Zero
	}
	if chargesTotal.IsNegative() {
		chargesTotal = decimal.Zero
	}
	if orderTax.IsPositive() {
		taxTotal = taxTotal.Add(orderTax)
	}
	subtotal = subtotal.Round(2)
	taxTotal = taxTotal.Round(2)
	discount = discount.Round(2)
	chargesTotal = chargesTotal.Round(2)

	raw := subtotal.Add(taxTotal).Sub(discount).Add(chargesTotal)
	if raw.IsNegative() {
		raw = decimal.Zero
	}
	total := raw.Ceil()
	return OrderTotals{
		Subtotal:      subtotal,
		TaxTotal:      taxTotal,
		DiscountTotal: discount,
		ChargesTotal:  chargesTotal,
		RoundOff:      total.Sub(raw).Round(2),
		TotalAmount:   total,
	}
}

// sumCharges totals an order-level charges map (packaging/service/shipping), ignoring
// non-positive entries.
func sumCharges(charges map[string]float64) decimal.Decimal {
	sum := decimal.Zero
	for _, v := range charges {
		if v > 0 {
			sum = sum.Add(decimal.NewFromFloat(v))
		}
	}
	return sum
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
	// printQueue enqueues background print jobs for the on-site Local Print Agent (AccuPOS model).
	printQueue *printing.Queue
}

// SetPublisher sets the event publisher for order lifecycle events.
func (s *Service) SetPublisher(p *events.Publisher) {
	s.publisher = p
}

// SetPrintQueue wires the background print-job queue (kitchen/bar tickets + customer bill).
func (s *Service) SetPrintQueue(q *printing.Queue) {
	s.printQueue = q
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

// CalculateTotals computes subtotal, tax, discount, and total for order lines using the flat
// fallback rate. Tax-inclusive lines (PriceIncludesTax) already contain their VAT inside the
// line price, so they are NEVER taxed again on top — mirroring the till's cart math
// (pos-ui src/lib/pos/cart-tax.ts). Prefer calculateTotalsWithTaxes when per-line treasury
// tax resolutions are available.
func (s *Service) CalculateTotals(lines []OrderLineInput, discountAmount decimal.Decimal) OrderTotals {
	subtotal := decimal.Zero
	taxableAmount := decimal.Zero

	for _, line := range lines {
		lineTotal := decimal.NewFromFloat(line.TotalPrice)
		if lineTotal.IsZero() {
			lineTotal = decimal.NewFromFloat(line.UnitPrice).Mul(decimal.NewFromFloat(line.Quantity))
		}
		subtotal = subtotal.Add(lineTotal)

		if (line.TaxStatus == "" || line.TaxStatus == "taxable") && !line.PriceIncludesTax {
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

// resolvedLineTax is the tax resolution for one order line, computed BEFORE the order is
// created so the header totals and the stored lines can never disagree.
type resolvedLineTax struct {
	CodeID    string
	KRACode   string
	Rate      float64 // VAT % (e.g. 16)
	Amount    float64 // line tax: embedded portion when inclusive, added portion when exclusive
	Inclusive bool
	HasInfo   bool // a definitive rate was resolved (treasury code, or the till sent its applied rate)
}

// resolveLineTaxes resolves tax for every line: the caller's explicit tax code → the local
// catalog projection default (POSCatalogOverride) → the till-provided rate (treasury-enriched
// catalog, carried on the create request). Lines marked tax_exempt / zero_rated resolve to a
// definitive zero so the fallback flat rate never taxes them.
func (s *Service) resolveLineTaxes(ctx context.Context, tenantID uuid.UUID, tenantSlug string, lines []OrderLineInput) []resolvedLineTax {
	out := make([]resolvedLineTax, len(lines))

	// Batch-load catalog tax defaults for all SKUs (synced from inventory-api ← treasury-api).
	type lineTaxDefault struct {
		code      string
		inclusive bool
	}
	taxBySKU := make(map[string]lineTaxDefault)
	skus := make([]string, 0, len(lines))
	for _, l := range lines {
		if l.SKU != "" {
			skus = append(skus, l.SKU)
		}
	}
	if len(skus) > 0 {
		overrides, _ := s.client.POSCatalogOverride.Query().
			Where(
				entoverride.TenantID(tenantID),
				entoverride.InventorySkuIn(skus...),
			).All(ctx)
		for _, o := range overrides {
			if o.TaxCodeID != "" {
				taxBySKU[o.InventorySku] = lineTaxDefault{code: o.TaxCodeID, inclusive: o.PriceIncludesTax}
			}
		}
	}

	for i, line := range lines {
		lineTotal := line.TotalPrice
		if lineTotal == 0 {
			lineTotal = line.UnitPrice * line.Quantity
		}
		r := resolvedLineTax{Inclusive: line.PriceIncludesTax}

		if line.TaxStatus == "tax_exempt" || line.TaxStatus == "zero_rated" {
			r.HasInfo = true // definitively untaxed — the flat fallback must not apply
			out[i] = r
			continue
		}

		lineTaxCode := line.TaxCodeID
		if lineTaxCode == "" {
			if d, ok := taxBySKU[line.SKU]; ok {
				lineTaxCode = d.code
				if !r.Inclusive {
					r.Inclusive = d.inclusive
				}
			}
		}

		if s.taxResolver != nil && lineTaxCode != "" {
			r.CodeID = lineTaxCode
			if tc, resolveErr := s.taxResolver.Resolve(ctx, tenantSlug, lineTaxCode); resolveErr == nil && tc != nil {
				r.Rate = tc.Rate
				r.KRACode = tc.KRACode
				r.Amount, _ = ComputeLineTax(lineTotal, r.Rate, r.Inclusive)
				r.HasInfo = true
			}
		}
		// No treasury code resolved but the till told us the rate it charged (from the
		// treasury-enriched catalog). Trust it — it is what the customer actually paid — so the
		// server's payable equals the till's payable. Includes an explicit 0 (non-VAT tenant).
		if !r.HasInfo && line.TaxRate != nil {
			r.Rate = *line.TaxRate
			r.Amount, _ = ComputeLineTax(lineTotal, r.Rate, r.Inclusive)
			r.HasInfo = true
		}
		out[i] = r
	}
	return out
}

// outletFallbackTaxRate returns the flat VAT fraction (e.g. 0.16) for lines with NO resolved tax
// info: the outlet's configured vat_rate (the SAME setting the till uses as its legacy fallback),
// else the service-level env default. VAT disabled on the outlet → zero.
func (s *Service) outletFallbackTaxRate(ctx context.Context, outletID uuid.UUID) decimal.Decimal {
	if set, err := s.client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(outletID)).
		Only(ctx); err == nil && set != nil {
		if !set.VatEnabled {
			return decimal.Zero
		}
		return decimal.NewFromFloat(set.VatRate).Div(decimal.NewFromInt(100))
	}
	return s.taxRate
}

// calculateTotalsWithTaxes computes order totals from per-line tax resolutions, mirroring the
// till's cart math exactly (pos-ui src/lib/pos/cart-tax.ts): subtotal is the gross rung-up
// amount; TaxTotal is only the tax ADDED on top (exclusive lines + flat fallback for lines with
// no tax info); inclusive lines contribute their embedded tax to the per-line record but never
// inflate the total. Order-level tax edits and additional charges land on top, and the payable
// is ceiled via finalizeTotals: total = subtotal + added tax − discount + charges + round_off.
func (s *Service) calculateTotalsWithTaxes(lines []OrderLineInput, taxes []resolvedLineTax, fallbackRate, discountAmount, chargesTotal, orderTax decimal.Decimal) OrderTotals {
	subtotal := decimal.Zero
	addedTax := decimal.Zero

	for i, line := range lines {
		lineTotal := decimal.NewFromFloat(line.TotalPrice)
		if lineTotal.IsZero() {
			lineTotal = decimal.NewFromFloat(line.UnitPrice).Mul(decimal.NewFromFloat(line.Quantity))
		}
		subtotal = subtotal.Add(lineTotal)

		t := taxes[i]
		switch {
		case t.HasInfo:
			if !t.Inclusive && t.Amount > 0 {
				addedTax = addedTax.Add(decimal.NewFromFloat(t.Amount))
			}
		case (line.TaxStatus == "" || line.TaxStatus == "taxable") && !line.PriceIncludesTax:
			if fallbackRate.IsPositive() {
				addedTax = addedTax.Add(lineTotal.Mul(fallbackRate))
			}
		}
	}

	return finalizeTotals(subtotal, addedTax, discountAmount, chargesTotal, orderTax)
}

// GenerateOrderNumber creates a unique order number.
func (s *Service) GenerateOrderNumber() string {
	return fmt.Sprintf("%s-%d", s.orderPrefix, time.Now().UnixMilli())
}

// deterministicOrderNumber derives a stable, collision-free order number from an offline
// client reference, so the same offline sale always maps to the same order number on
// every sync attempt (and two sales rung up in the same millisecond never collide).
func (s *Service) deterministicOrderNumber(clientRef string) string {
	sum := sha256.Sum256([]byte(clientRef))
	return fmt.Sprintf("%s-%s", s.orderPrefix, strings.ToUpper(hex.EncodeToString(sum[:6])))
}

// DefaultCurrency returns the configured default currency.
func (s *Service) DefaultCurrency() string {
	return s.defaultCurrency
}

// CreateOrder creates a new POS order with proper tax/discount calculation.
//
// Idempotency: when req.ClientReference is set (an offline-created sale), this is
// get-or-create. If an order already exists for (tenant_id, client_reference) we return
// it unchanged BEFORE any side effects (lines, commit, order.created event, stock
// deduction), so a replayed sync never duplicates the order, double-deducts stock, or
// double-publishes. The unique (tenant_id, client_reference) index is the final backstop.
func (s *Service) CreateOrder(ctx context.Context, req CreateOrderRequest) (*ent.POSOrder, error) {
	if req.ClientReference != "" {
		if existing, qerr := s.client.POSOrder.Query().
			Where(posorder.TenantID(req.TenantID), posorder.ClientReference(req.ClientReference)).
			WithLines().
			Only(ctx); qerr == nil && existing != nil {
			return existing, nil
		}
	}

	currency := req.Currency
	if currency == "" {
		currency = s.defaultCurrency
	}
	orderNumber := req.OrderNumber
	if orderNumber == "" {
		// For offline sales derive a deterministic, collision-free number from the client
		// reference so two sales rung up in the same millisecond can't share a number
		// (GenerateOrderNumber is time-based). Online sales keep the time-based number.
		if req.ClientReference != "" {
			orderNumber = s.deterministicOrderNumber(req.ClientReference)
		} else {
			orderNumber = s.GenerateOrderNumber()
		}
	}

	// Non-billable / complimentary lines (free accompaniments like ugali, supplies like
	// packaging — flagged by the catalog) are NEVER charged, even if a client sends a
	// price: force them to zero before totals/tax so the payable can't include them.
	// Their stock still deducts (the inventory consumer deducts by SKU regardless of price).
	for i := range req.Lines {
		if lineIsNonBillable(req.Lines[i].Metadata) {
			req.Lines[i].UnitPrice = 0
			req.Lines[i].TotalPrice = 0
		}
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
	// Resolve every line's tax BEFORE computing totals so the order header and its lines can
	// never disagree — the historical flat-16%-on-top header math made every sale under a
	// tax-inclusive tenant "partially paid" by exactly the phantom tax.
	lineTaxes := s.resolveLineTaxes(ctx, req.TenantID, req.TenantSlug, req.Lines)
	orderTax := decimal.NewFromFloat(req.OrderTaxAmount)
	totals := s.calculateTotalsWithTaxes(req.Lines, lineTaxes, s.outletFallbackTaxRate(ctx, req.OutletID), discount, sumCharges(req.Charges), orderTax)

	// Resolve order subtype, defaulting to dine_in. "draft" is a status, not a subtype —
	// the Save-as-Draft flows send it here, so normalize it to retail (retail orders start
	// in draft status anyway, see initialStatus below). Anything else outside the enum is a
	// client error, surfaced as ErrInvalidOrderSubtype instead of a 500 from Ent validation.
	subtype := strings.ToLower(strings.TrimSpace(req.OrderSubtype))
	switch {
	case subtype == "":
		subtype = "dine_in"
	case subtype == "draft":
		subtype = "retail"
	default:
		if _, ok := validOrderSubtypes[subtype]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrInvalidOrderSubtype, req.OrderSubtype)
		}
	}

	// Carry table_id in metadata (no dedicated DB column yet).
	meta := req.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	if req.TableID != "" {
		meta["table_id"] = req.TableID
	}
	// Order-level adjustments audit trail: per-charge breakdown + the manual order-tax edit
	// (the amounts themselves are inside charges_total / tax_total).
	if totals.ChargesTotal.IsPositive() && req.Charges != nil {
		charges := map[string]any{}
		for k, v := range req.Charges {
			if v > 0 {
				charges[k] = v
			}
		}
		meta["charges"] = charges
	}
	if orderTax.IsPositive() {
		meta["order_tax"] = map[string]any{"amount": orderTax.InexactFloat64()}
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

	// Kitchen-routed subtypes are opened immediately so the kitchen receives a KDS ticket as
	// soon as the order is placed. This includes DELIVERY and TAKEAWAY: both need the kitchen to
	// prepare the food (delivery is then dispatched to a rider, takeaway is packed for pickup).
	// Only "retail" (non-prepared goods) stays a draft until paid.
	initialStatus := StatusDraft
	isHospitalityOrder := subtype == "dine_in" || subtype == "takeaway" || subtype == "room_service" || subtype == "bar_tab" || subtype == "delivery"
	if isHospitalityOrder {
		initialStatus = StatusOpen
	}

	// source defaults to pos_terminal; the back-office Add Sale flow passes "back_office".
	source := req.Source
	if source == "" {
		source = "pos_terminal"
	}

	orderBuilder := tx.POSOrder.Create().
		SetTenantID(req.TenantID).
		SetOutletID(req.OutletID).
		SetDeviceID(req.DeviceID).
		SetUserID(req.UserID).
		SetOrderNumber(orderNumber).
		SetStatus(initialStatus).
		SetSource(source).
		SetSubtotal(totals.Subtotal.InexactFloat64()).
		SetTaxTotal(totals.TaxTotal.InexactFloat64()).
		SetDiscountTotal(totals.DiscountTotal.InexactFloat64()).
		SetChargesTotal(totals.ChargesTotal.InexactFloat64()).
		SetRoundOff(totals.RoundOff.InexactFloat64()).
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
	if req.ClientReference != "" {
		orderBuilder = orderBuilder.SetClientReference(req.ClientReference)
	}
	if req.OfflineCreatedAt != nil {
		orderBuilder = orderBuilder.SetOfflineCreatedAt(*req.OfflineCreatedAt)
	}
	order, err := orderBuilder.Save(ctx)
	if err != nil {
		// Lost a race against a concurrent replay of the same offline sale: the unique
		// (tenant_id, client_reference) index rejected the duplicate. Return the winner.
		if req.ClientReference != "" && ent.IsConstraintError(err) {
			_ = tx.Rollback()
			if existing, qerr := s.client.POSOrder.Query().
				Where(posorder.TenantID(req.TenantID), posorder.ClientReference(req.ClientReference)).
				WithLines().
				Only(ctx); qerr == nil && existing != nil {
				return existing, nil
			}
		}
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
			).All(ctx)
		for _, o := range overrides {
			if o.KdsStationID != nil {
				kdsOverrideBySKU[o.InventorySku] = *o.KdsStationID
			}
		}
	}

	for li, line := range req.Lines {
		lineTotal := decimal.NewFromFloat(line.TotalPrice)
		if lineTotal.IsZero() {
			lineTotal = decimal.NewFromFloat(line.UnitPrice).Mul(decimal.NewFromFloat(line.Quantity))
		}
		meta := line.Metadata
		if meta == nil {
			meta = map[string]any{}
		}

		// Tax was resolved up-front (resolveLineTaxes) — the same numbers the header totals used.
		lt := lineTaxes[li]
		taxCodeID, taxKraCode := lt.CodeID, lt.KRACode
		taxRate, taxAmt := lt.Rate, lt.Amount
		priceIncludesTax := lt.Inclusive

		lineCreate := tx.POSOrderLine.Create().
			SetOrderID(order.ID).
			SetCatalogItemID(line.CatalogItemID).
			SetSku(line.SKU).
			SetName(line.Name).
			SetCategory(line.Category).
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
		// Background printing (AccuPOS model): enqueue kitchen/bar tickets + customer bill for the
		// outlet's Local Print Agent so the till never blocks on (or re-does) printing.
		s.enqueueAutoPrintJobs(ctx, req.TenantID, result)
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

// RequestSaleNotification publishes pos.sale.notification_requested for an order — the
// All-Sales "New Sale Notification" action. notifications-service consumes it and sends the
// customer their receipt/invoice (SMS/email/WhatsApp). It does NOT re-post to the ledger.
// overridePhone/overrideEmail let the cashier redirect the notification if the order has none.
func (s *Service) RequestSaleNotification(ctx context.Context, tenantID, orderID uuid.UUID, overridePhone, overrideEmail string) (*ent.POSOrder, error) {
	order, err := s.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tenantID)).
		WithLines().
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("orders: not found: %w", err)
	}

	phone := overridePhone
	if phone == "" && order.CustomerPhone != nil {
		phone = *order.CustomerPhone
	}
	name := ""
	if order.CustomerName != nil {
		name = *order.CustomerName
	}
	items := make([]map[string]any, 0, len(order.Edges.Lines))
	for _, l := range order.Edges.Lines {
		items = append(items, map[string]any{
			"name": l.Name, "quantity": l.Quantity, "total_price": l.TotalPrice,
		})
	}

	if s.publisher != nil {
		_ = s.publisher.PublishSaleNotificationRequested(ctx, tenantID, map[string]any{
			"order_id":       orderID.String(),
			"order_number":   order.OrderNumber,
			"tenant_id":      tenantID.String(),
			"outlet_id":      order.OutletID.String(),
			"customer_name":  name,
			"customer_phone": phone,
			"customer_email": overrideEmail,
			"total_amount":   order.TotalAmount,
			"currency":       order.Currency,
			"items":          items,
			"etims_invoice_number": derefStr(order.EtimsInvoiceNumber),
		})
	}
	return order, nil
}

// derefStr returns the pointed-to string or "" for a nil pointer.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
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
//  2. Station category_filter — strict exact match of the item's category (name substring only
//     when the item has no category)
//  3. Expo / "all" stations — receive every item as a secondary copy for the expediter
//
// A station with station_type="expo" or "all" always receives EVERY item.
// Items with no explicit station and no matching category_filter go to expo/all stations;
// if no such station exists they go to the first active station.
func routeLinesToStations(lines []*ent.POSOrderLine, stations []*ent.KDSStation) map[uuid.UUID][]map[string]any {
	stationItems := make(map[uuid.UUID][]map[string]any, len(stations))

	// Identify expo/all and the first kitchen station upfront.
	var expoIDs []uuid.UUID
	var kitchenID *uuid.UUID
	for _, st := range stations {
		if st.StationType == "expo" || st.StationType == "all" {
			expoIDs = append(expoIDs, st.ID)
		}
		if st.StationType == "kitchen" && kitchenID == nil {
			id := st.ID
			kitchenID = &id
		}
	}

	for _, l := range lines {
		item := map[string]any{
			"sku":      l.Sku,
			"name":     l.Name,
			"quantity": l.Quantity,
		}

		routed := false

		// Priority 1: explicit station on the order line (set from catalog override) — manager wins.
		if l.KdsStationID != nil {
			stationItems[*l.KdsStationID] = append(stationItems[*l.KdsStationID], item)
			routed = true
		}

		// Coffee & tea (and other hot beverages) are kitchen items, never bar — the bar prepares
		// alcohol and cold drinks. Force these to the kitchen station when one exists, before the
		// category_filter fallback can capture them for a "beverages" bar station.
		isHot := isHotBeverage(l.Name, l.Category)
		if !routed && isHot && kitchenID != nil {
			stationItems[*kitchenID] = append(stationItems[*kitchenID], item)
			routed = true
		}

		// Priority 2: strict category_filter match. The item's CATEGORY (stamped from the live
		// inventory catalog at sale time) must EXACTLY equal one of the station's category filters
		// (case-insensitive, trimmed). Because each category is claimed by exactly one station
		// (enforced on station create/update), this routes every ticket to a single, correct
		// destination. Only when the line carries no category (legacy/uncategorized item) do we
		// fall back to a substring match on the item name. Bar stations are skipped for hot
		// beverages so coffee/tea can't be dragged to the bar by a "beverages" filter.
		if !routed {
			itemCat := strings.ToLower(strings.TrimSpace(l.Category))
			itemName := strings.ToLower(l.Name)
			for _, st := range stations {
				if st.StationType == "expo" || st.StationType == "all" {
					continue // handled separately below
				}
				if isHot && st.StationType == "bar" {
					continue
				}
				for _, cat := range st.CategoryFilter {
					needle := strings.ToLower(strings.TrimSpace(cat))
					if needle == "" {
						continue
					}
					matched := itemCat != "" && itemCat == needle ||
						itemCat == "" && strings.Contains(itemName, needle)
					if matched {
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

// hotBeverageKeywords are matched (case-insensitive substring) against an item's name and category
// to keep coffee/tea-type drinks on the kitchen station rather than the bar.
var hotBeverageKeywords = []string{
	"coffee", "tea", "espresso", "cappuccino", "latte", "americano", "macchiato",
	"mocha", "hot chocolate", "chai", "flat white", "cortado", "affogato",
	"hot beverage", "hot drink",
}

// isHotBeverage reports whether an item (by name or category) is a hot beverage that should be
// prepared in the kitchen, not the bar.
func isHotBeverage(name, category string) bool {
	hay := strings.ToLower(name + " " + category)
	for _, kw := range hotBeverageKeywords {
		if strings.Contains(hay, kw) {
			// Guard against false positives like "iced tea"/"iced coffee" which are bar/cold drinks.
			if (kw == "coffee" || kw == "tea") && (strings.Contains(hay, "iced "+kw) || strings.Contains(hay, "ice "+kw)) {
				continue
			}
			return true
		}
	}
	return false
}

// createKDSTicketsForOrder creates per-station KDS tickets with only the items
// relevant to each station. Items are routed via kds_station_id on the order line
// (resolved from POSCatalogOverride at order creation) with a category_filter
// keyword fallback. Expo/all stations receive every item as a secondary copy.
func (s *Service) createKDSTicketsForOrder(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder) error {
	// Printer-only kitchen: when the outlet has NO Kitchen Display System (enable_kds=false), do
	// not create persistent KDS tickets. There's no screen/device to bump them, so they would pile
	// up forever — the classic single-terminal + kitchen-printer setup. The kitchen works off the
	// printed chit (auto_print_kitchen) and the order is served/settled from the POS terminal.
	// (A missing settings row keeps the previous behaviour — create tickets.)
	if setting, sErr := s.client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(order.OutletID)).Only(ctx); sErr == nil && !setting.EnableKds {
		return nil
	}

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
			SetOrderSubtype(string(order.OrderSubtype)).
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
func (s *Service) AddOrderLines(ctx context.Context, tenantID uuid.UUID, tenantSlug string, orderID uuid.UUID, lines []OrderLineInput) (*ent.POSOrder, error) {
	order, err := s.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tenantID)).
		WithLines().
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("orders: order not found: %w", err)
	}
	// Allow adding to any unpaid, non-terminal order (draft / open / pending_payment) — "add to bill"
	// works as long as the order isn't already settled or closed. A bill awaiting payment is re-opened
	// when new items are added (there is now more to pay).
	switch order.Status {
	case StatusCompleted, StatusCancelled, StatusVoided, StatusRefunded:
		return nil, fmt.Errorf("orders: cannot add items to a %s order", order.Status)
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

	// Resolve tax for the new lines the same way CreateOrder does, so add-to-bill lines carry
	// their VAT and the recomputed header stays consistent with the till.
	lineTaxes := s.resolveLineTaxes(ctx, tenantID, tenantSlug, lines)

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

	for li, l := range lines {
		lineTotal := decimal.NewFromFloat(l.TotalPrice)
		if lineTotal.IsZero() {
			lineTotal = decimal.NewFromFloat(l.UnitPrice).Mul(decimal.NewFromFloat(l.Quantity))
		}
		meta := l.Metadata
		if meta == nil {
			meta = map[string]any{}
		}
		lt := lineTaxes[li]
		lc := tx.POSOrderLine.Create().
			SetOrderID(order.ID).
			SetCatalogItemID(l.CatalogItemID).
			SetSku(l.SKU).
			SetName(l.Name).
			SetCategory(l.Category).
			SetQuantity(l.Quantity).
			SetUnitPrice(l.UnitPrice).
			SetTotalPrice(lineTotal.InexactFloat64()).
			SetPriceIncludesTax(lt.Inclusive).
			SetCourseNumber(l.CourseNumber).
			SetMetadata(meta)
		if lt.CodeID != "" {
			lc = lc.SetTaxCodeID(lt.CodeID)
		}
		if lt.KRACode != "" {
			lc = lc.SetTaxKraCode(lt.KRACode)
		}
		if lt.Rate > 0 {
			lc = lc.SetTaxRate(lt.Rate)
		}
		if lt.Amount > 0 {
			lc = lc.SetTaxAmount(lt.Amount)
		}
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

	// Recalculate totals from all lines (existing + new). Only tax on EXCLUSIVE lines is added
	// on top — an inclusive line's tax_amount is already inside its total_price, and adding it
	// again would inflate the payable above what the till charged. The order's stored discount,
	// charges and manual order-tax edit are carried through finalizeTotals so the ceiling
	// identity (total = subtotal + tax − discount + charges + round_off) keeps holding.
	allLines := append(order.Edges.Lines, newLines...)
	var newSubtotal, newTaxTotal decimal.Decimal
	for _, ol := range allLines {
		newSubtotal = newSubtotal.Add(decimal.NewFromFloat(ol.TotalPrice))
		if ol.TaxAmount != nil && !ol.PriceIncludesTax {
			newTaxTotal = newTaxTotal.Add(decimal.NewFromFloat(*ol.TaxAmount))
		}
	}
	orderTax := decimal.Zero
	if ot, ok := order.Metadata["order_tax"].(map[string]any); ok {
		if amt, ok := ot["amount"].(float64); ok && amt > 0 {
			orderTax = decimal.NewFromFloat(amt)
		}
	}
	totals := finalizeTotals(newSubtotal, newTaxTotal, decimal.NewFromFloat(order.DiscountTotal), decimal.NewFromFloat(order.ChargesTotal), orderTax)

	upd := tx.POSOrder.UpdateOneID(order.ID).
		SetSubtotal(totals.Subtotal.InexactFloat64()).
		SetTaxTotal(totals.TaxTotal.InexactFloat64()).
		SetRoundOff(totals.RoundOff.InexactFloat64()).
		SetTotalAmount(totals.TotalAmount.InexactFloat64())
	// Adding items to a bill that was awaiting payment (or still a draft) re-opens it — there is now
	// more to pay, so it must not stay in a pending/draft state.
	if order.Status == StatusPendingPayment || order.Status == StatusDraft {
		upd = upd.SetStatus(StatusOpen)
	}
	if _, err = upd.Save(ctx); err != nil {
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
			SetOrderSubtype(string(order.OrderSubtype)).
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
			SetOrderSubtype(string(order.OrderSubtype)).
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
