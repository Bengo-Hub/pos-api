// Package promotions provides the promotion service layer for POS operations.
// It encapsulates promo code validation, discount calculation, and usage tracking
// that was previously incomplete in handlers (only validated existence, never calculated discounts).
package promotions

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/promotion"
	"github.com/bengobox/pos-service/internal/ent/promotionrule"
)

// ApplyResult holds the result of applying a promotion to an order.
type ApplyResult struct {
	Valid          bool            `json:"valid"`
	PromoCode      string          `json:"promo_code"`
	PromoID        uuid.UUID       `json:"promo_id"`
	DiscountAmount decimal.Decimal `json:"discount_amount"`
	Reason         string          `json:"reason,omitempty"` // reason for invalid
}

// Service provides promotion business logic.
type Service struct {
	client *ent.Client
	log    *zap.Logger
}

// NewService creates a new promotion service.
func NewService(client *ent.Client, log *zap.Logger) *Service {
	return &Service{
		client: client,
		log:    log.Named("promotions.service"),
	}
}

// ApplyPromoCode validates a promo code and calculates the discount amount.
// Unlike the previous implementation which only checked existence, this:
// 1. Validates the promo code exists and is active
// 2. Checks expiry
// 3. Checks usage limits (if configured via metadata)
// 4. Calculates the discount amount based on promo metadata
func (s *Service) ApplyPromoCode(ctx context.Context, tenantID uuid.UUID, promoCode string, orderAmount decimal.Decimal) (*ApplyResult, error) {
	promo, err := s.client.Promotion.Query().
		Where(
			promotion.TenantID(tenantID),
			promotion.PromoCode(promoCode),
			promotion.StatusEQ("active"),
		).
		Only(ctx)
	if err != nil {
		return &ApplyResult{Valid: false, Reason: "promo code not found or inactive"}, nil
	}

	code := derefStr(promo.PromoCode)

	// Check expiry
	if promo.EndAt != nil && time.Now().After(*promo.EndAt) {
		return &ApplyResult{Valid: false, PromoCode: code, PromoID: promo.ID, Reason: "promotion has expired"}, nil
	}

	// Check start date
	if !promo.StartAt.IsZero() && time.Now().Before(promo.StartAt) {
		return &ApplyResult{Valid: false, PromoCode: code, PromoID: promo.ID, Reason: "promotion has not started yet"}, nil
	}

	// Calculate discount from metadata
	discountAmount := s.calculateDiscount(promo, orderAmount)

	return &ApplyResult{
		Valid:          true,
		PromoCode:      code,
		PromoID:        promo.ID,
		DiscountAmount: discountAmount.Round(2),
	}, nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// calculateDiscount determines the discount amount based on promotion metadata.
// Supports: {"discount_type": "percentage|fixed", "discount_value": 10.0, "max_discount": 500.0}
func (s *Service) calculateDiscount(promo *ent.Promotion, orderAmount decimal.Decimal) decimal.Decimal {
	meta := promo.Metadata
	if meta == nil {
		return decimal.Zero
	}

	discountType, _ := meta["discount_type"].(string)
	discountValue, _ := meta["discount_value"].(float64)

	if discountValue <= 0 {
		return decimal.Zero
	}

	var discount decimal.Decimal
	switch discountType {
	case "percentage":
		discount = orderAmount.Mul(decimal.NewFromFloat(discountValue)).Div(decimal.NewFromInt(100))
	case "fixed":
		discount = decimal.NewFromFloat(discountValue)
	default:
		return decimal.Zero
	}

	// Cap at max_discount if specified
	if maxDiscount, ok := meta["max_discount"].(float64); ok && maxDiscount > 0 {
		maxDec := decimal.NewFromFloat(maxDiscount)
		if discount.GreaterThan(maxDec) {
			discount = maxDec
		}
	}

	// Don't exceed order amount
	if discount.GreaterThan(orderAmount) {
		discount = orderAmount
	}

	return discount
}

// outletTimezone resolves an outlet's configured IANA timezone (schema default
// "Africa/Nairobi"), falling back to that default on any lookup/parse error so a bad or
// missing outlet_id never breaks schedule evaluation — it just falls back to the platform's
// home timezone rather than the container's UTC clock.
func (s *Service) outletTimezone(ctx context.Context, outletID *uuid.UUID) *time.Location {
	const fallback = "Africa/Nairobi"
	tz := fallback
	if outletID != nil {
		if o, err := s.client.Outlet.Get(ctx, *outletID); err == nil && o.Timezone != "" {
			tz = o.Timezone
		}
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc, _ = time.LoadLocation(fallback)
	}
	if loc == nil {
		loc = time.UTC
	}
	return loc
}

// ActiveHappyHours returns auto-apply happy-hour promotions that are live at `now`
// for the given outlet (nil outlet promos apply to all outlets). A promo is live when:
//   - promo_kind = happy_hour, status = active, auto_apply = true
//   - now is within [start_at, end_at] (date range, if set)
//   - now's weekday is in days_of_week (or days_of_week empty = every day)
//   - now's HH:MM is within [window_start, window_end]
//
// Schedule fields (days_of_week/window_start/window_end) are configured by the tenant in the
// OUTLET'S LOCAL wall-clock time (e.g. "18:00" means 6pm Nairobi time), so `now` — which
// callers pass as a UTC timestamp (server clock / order.created_at) — MUST be converted to the
// outlet's timezone before comparison. Comparing raw UTC against locally-configured window
// strings silently shifts the effective window by the timezone offset (bug fixed 2026-07-11:
// a "18:00-23:00" EAT window was being matched against the pod's UTC clock, so happy-hour
// orders placed at ~19:00 Nairobi time were rejected because the pod still read ~16:00 UTC).
func (s *Service) ActiveHappyHours(ctx context.Context, tenantID uuid.UUID, outletID *uuid.UUID, now time.Time) ([]*ent.Promotion, error) {
	q := s.client.Promotion.Query().Where(
		promotion.TenantID(tenantID),
		promotion.PromoKindEQ(promotion.PromoKindHappyHour),
		promotion.StatusEQ("active"),
		promotion.AutoApply(true),
	)
	promos, err := q.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("promotions: query happy hours: %w", err)
	}
	loc := s.outletTimezone(ctx, outletID)
	localNow := now.In(loc)
	var active []*ent.Promotion
	for _, p := range promos {
		if p.OutletID != nil && (outletID == nil || *p.OutletID != *outletID) {
			continue
		}
		if !s.isWithinSchedule(p, localNow) {
			continue
		}
		active = append(active, p)
	}
	return active, nil
}

// EvaluateHappyHourDiscount returns the best auto-apply happy-hour discount for an outlet
// on the given subtotal at `now`. Used by the orders service at checkout (decoupled hook).
func (s *Service) EvaluateHappyHourDiscount(ctx context.Context, tenantID, outletID uuid.UUID, subtotal decimal.Decimal) decimal.Decimal {
	if subtotal.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	var outletPtr *uuid.UUID
	if outletID != uuid.Nil {
		outletPtr = &outletID
	}
	active, err := s.ActiveHappyHours(ctx, tenantID, outletPtr, time.Now())
	if err != nil || len(active) == 0 {
		return decimal.Zero
	}
	best := decimal.Zero
	for _, p := range active {
		rule, rErr := s.client.PromotionRule.Query().
			Where(promotionrule.PromotionID(p.ID)).First(ctx)
		if rErr != nil || rule == nil {
			continue
		}
		var d decimal.Decimal
		switch rule.DiscountType {
		case "percentage":
			d = subtotal.Mul(decimal.NewFromFloat(rule.DiscountValue)).Div(decimal.NewFromInt(100))
		case "fixed_amount":
			d = decimal.NewFromFloat(rule.DiscountValue)
		default:
			continue
		}
		if rule.MaxDiscount != nil && *rule.MaxDiscount > 0 {
			if cap := decimal.NewFromFloat(*rule.MaxDiscount); d.GreaterThan(cap) {
				d = cap
			}
		}
		if d.GreaterThan(best) {
			best = d
		}
	}
	if best.GreaterThan(subtotal) {
		best = subtotal
	}
	return best.Round(2)
}

// DiscountLine is the minimal order-line info the evaluator needs to enforce scope.
// Quantity/UnitPrice are only needed for discount_type=bogo (percentage/fixed_amount/
// fixed_price work off Total alone); Category is used for scope_type=category. Other callers
// may leave the extra fields zero.
type DiscountLine struct {
	SKU       string
	Category  string
	Total     decimal.Decimal
	Quantity  float64
	UnitPrice decimal.Decimal
}

// LineDiscount is the per-SKU breakdown of the winning auto-apply discount, so the caller can
// stamp each order line with a human-readable happy-hour annotation (e.g. "Buy 1 Get 1 Free" +
// the KES amount saved) shown on the terminal, the bill, and the printed receipt.
type LineDiscount struct {
	SKU     string          `json:"sku"`
	Amount  decimal.Decimal `json:"amount"`
	FreeQty float64         `json:"free_qty"`
	Type    string          `json:"type"`
	Label   string          `json:"label"`
}

// AutoDiscountResult is the winning auto-apply discount for an order (best of all active promos).
type AutoDiscountResult struct {
	PromoID   uuid.UUID
	PromoName string
	Discount  decimal.Decimal
	// PerSKU maps a scoped SKU → its share of the discount + a display label. Empty for
	// storewide percentage/fixed discounts that don't attribute to specific lines.
	PerSKU map[string]LineDiscount
}

// EvaluateAutoDiscount returns the best auto-apply (happy-hour / negotiated meal) discount for an
// outlet at now, ENFORCING each rule's scope and discount type. This fixes the prior gaps:
//   - scope_type=item → only lines whose SKU is in scope_ids are discounted (e.g. a lunch/beverages deal)
//   - discount_type=fixed_price → the scoped lines are repriced to discount_value (negotiated meal price)
//   - returns the promo id so the caller can write a PromotionApplication audit row
//
// scope_type=category is treated as "all" here (best-effort) since order lines don't carry category;
// category targeting is handled upstream when lines are tagged. meal_period is informational metadata
// used for reporting/targeting and does not change the math (the negotiated rate is the discount itself).
func (s *Service) EvaluateAutoDiscount(ctx context.Context, tenantID, outletID uuid.UUID, lines []DiscountLine) AutoDiscountResult {
	subtotal := decimal.Zero
	for _, l := range lines {
		subtotal = subtotal.Add(l.Total)
	}
	if subtotal.LessThanOrEqual(decimal.Zero) {
		return AutoDiscountResult{}
	}
	var outletPtr *uuid.UUID
	if outletID != uuid.Nil {
		outletPtr = &outletID
	}
	active, err := s.ActiveHappyHours(ctx, tenantID, outletPtr, time.Now())
	if err != nil || len(active) == 0 {
		return AutoDiscountResult{}
	}

	best := AutoDiscountResult{}
	for _, p := range active {
		rule, rErr := s.client.PromotionRule.Query().Where(promotionrule.PromotionID(p.ID)).First(ctx)
		if rErr != nil || rule == nil {
			continue
		}

		d, perSKU := s.evaluateRule(rule, lines, subtotal)
		if d.GreaterThan(best.Discount) {
			best = AutoDiscountResult{PromoID: p.ID, PromoName: p.Name, Discount: d, PerSKU: perSKU}
		}
	}
	if best.Discount.GreaterThan(subtotal) {
		best.Discount = subtotal
	}
	best.Discount = best.Discount.Round(2)
	return best
}

// evaluateRule computes the discount for a single rule against the cart, returning the total
// AND a per-SKU breakdown (amount + display label) so the caller can annotate each order line.
func (s *Service) evaluateRule(rule *ent.PromotionRule, lines []DiscountLine, subtotal decimal.Decimal) (decimal.Decimal, map[string]LineDiscount) {
	// BOGO is quantity-driven (per-SKU pairing), not a fraction of a base total.
	if rule.DiscountType == promotionrule.DiscountTypeBogo {
		return s.calculateBOGODiscount(rule, lines)
	}

	// Resolve which lines are in scope. scope_type=item matches SKU; scope_type=category
	// matches the line's category (order lines DO carry category at sale time); anything
	// else (storewide) discounts the whole cart.
	inScope := func(l DiscountLine) bool { return true }
	scoped := false
	if len(rule.ScopeIds) > 0 {
		set := map[string]struct{}{}
		for _, id := range rule.ScopeIds {
			set[strings.ToLower(strings.TrimSpace(id))] = struct{}{}
		}
		switch rule.ScopeType {
		case promotionrule.ScopeTypeItem:
			scoped = true
			inScope = func(l DiscountLine) bool { _, ok := set[strings.ToLower(strings.TrimSpace(l.SKU))]; return ok }
		case promotionrule.ScopeTypeCategory:
			scoped = true
			inScope = func(l DiscountLine) bool { _, ok := set[strings.ToLower(strings.TrimSpace(l.Category))]; return ok }
		}
	}

	base := decimal.Zero
	for _, l := range lines {
		if inScope(l) {
			base = base.Add(l.Total)
		}
	}
	if base.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero, nil
	}

	var total decimal.Decimal
	var label string
	switch rule.DiscountType {
	case "percentage":
		total = base.Mul(decimal.NewFromFloat(rule.DiscountValue)).Div(decimal.NewFromInt(100))
		label = fmt.Sprintf("%s%% off", trimNum(rule.DiscountValue))
	case "fixed_amount":
		total = decimal.NewFromFloat(rule.DiscountValue)
		label = fmt.Sprintf("KES %s off", trimNum(rule.DiscountValue))
	case "fixed_price":
		total = base.Sub(decimal.NewFromFloat(rule.DiscountValue))
		if total.IsNegative() {
			total = decimal.Zero
		}
		label = fmt.Sprintf("Fixed price KES %s", trimNum(rule.DiscountValue))
	default:
		return decimal.Zero, nil
	}
	if rule.MaxDiscount != nil && *rule.MaxDiscount > 0 {
		if cap := decimal.NewFromFloat(*rule.MaxDiscount); total.GreaterThan(cap) {
			total = cap
		}
	}
	if total.GreaterThan(base) {
		total = base
	}

	// Attribute the discount across scoped SKUs proportionally to their line totals so the
	// receipt can show each item's share. Storewide (unscoped) discounts have no per-item
	// attribution — they surface only as the order-level discount line.
	perSKU := map[string]LineDiscount{}
	if scoped && total.IsPositive() {
		totalBySKU := map[string]decimal.Decimal{}
		for _, l := range lines {
			if inScope(l) {
				totalBySKU[l.SKU] = totalBySKU[l.SKU].Add(l.Total)
			}
		}
		for sku, skuTotal := range totalBySKU {
			if skuTotal.LessThanOrEqual(decimal.Zero) {
				continue
			}
			share := total.Mul(skuTotal).Div(base).Round(2)
			perSKU[sku] = LineDiscount{SKU: sku, Amount: share, Type: string(rule.DiscountType), Label: label}
		}
	}
	return total, perSKU
}

// trimNum renders a float without a trailing ".0" (e.g. 20 not 20.0, 12.5 stays 12.5).
func trimNum(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// calculateBOGODiscount computes a "buy X get Y [at N% off]" discount: for every
// buy_quantity units of a scoped SKU in the cart, get_quantity more units of the SAME SKU
// are discounted by get_discount_percent (100 = fully free — the classic "buy one get one
// free"). Grouped per-SKU (a cart can carry the same SKU split across multiple lines — one
// pre-existing, one just added) rather than pooled across different scoped SKUs, matching
// how a real "buy 1 burger get 1 burger free" deal reads: adding a second UNRELATED scoped
// item never completes someone else's pair. scope_type must be "item" with scope_ids set —
// a storewide/category BOGO has no well-defined "unit" to pair, so it's a no-op.
func (s *Service) calculateBOGODiscount(rule *ent.PromotionRule, lines []DiscountLine) (decimal.Decimal, map[string]LineDiscount) {
	if rule.ScopeType != promotionrule.ScopeTypeItem || len(rule.ScopeIds) == 0 {
		return decimal.Zero, nil
	}
	inScope := map[string]struct{}{}
	for _, id := range rule.ScopeIds {
		inScope[strings.ToLower(strings.TrimSpace(id))] = struct{}{}
	}
	buyQty := rule.BuyQuantity
	if buyQty <= 0 {
		buyQty = 1
	}
	getQty := rule.GetQuantity
	if getQty <= 0 {
		getQty = 1
	}
	getPct := rule.GetDiscountPercent
	if getPct <= 0 {
		getPct = 100
	}
	cycle := buyQty + getQty

	qtyBySKU := map[string]float64{}
	priceBySKU := map[string]decimal.Decimal{}
	for _, l := range lines {
		if _, ok := inScope[strings.ToLower(strings.TrimSpace(l.SKU))]; !ok {
			continue
		}
		qtyBySKU[l.SKU] += l.Quantity
		// Assume a uniform unit price per SKU across split lines (true for every caller
		// today — modifiers/variants carry their own SKU, so a price mismatch here would
		// mean two different priced items sharing one SKU, which is itself a data bug).
		priceBySKU[l.SKU] = l.UnitPrice
	}

	freeLabel := "Free"
	if getPct < 100 {
		freeLabel = trimNum(getPct) + "% off"
	}
	label := fmt.Sprintf("Buy %d Get %d %s", buyQty, getQty, freeLabel)

	total := decimal.Zero
	perSKU := map[string]LineDiscount{}
	for sku, qty := range qtyBySKU {
		pairs := int(qty) / cycle
		if pairs <= 0 {
			continue
		}
		freeUnits := float64(pairs * getQty)
		amt := priceBySKU[sku].Mul(decimal.NewFromFloat(freeUnits)).Mul(decimal.NewFromFloat(getPct / 100)).Round(2)
		total = total.Add(amt)
		perSKU[sku] = LineDiscount{SKU: sku, Amount: amt, FreeQty: freeUnits, Type: "bogo", Label: label}
	}
	if rule.MaxDiscount != nil && *rule.MaxDiscount > 0 {
		if cap := decimal.NewFromFloat(*rule.MaxDiscount); total.GreaterThan(cap) {
			total = cap
		}
	}
	return total, perSKU
}

// RecordApplication writes a PromotionApplication audit row linking a promo to the order it discounted.
func (s *Service) RecordApplication(ctx context.Context, promoID, orderID uuid.UUID, amount decimal.Decimal) {
	if promoID == uuid.Nil || amount.LessThanOrEqual(decimal.Zero) {
		return
	}
	if _, err := s.client.PromotionApplication.Create().
		SetPromotionID(promoID).
		SetOrderID(orderID).
		SetDiscountAmount(amount.InexactFloat64()).
		Save(ctx); err != nil {
		s.log.Warn("failed to record promotion application", zap.Error(err))
	}
}

// isWithinSchedule reports whether `now` falls inside a promotion's date range,
// allowed weekdays, and daily time window.
func (s *Service) isWithinSchedule(p *ent.Promotion, now time.Time) bool {
	if !p.StartAt.IsZero() && now.Before(p.StartAt) {
		return false
	}
	if p.EndAt != nil && now.After(*p.EndAt) {
		return false
	}
	if len(p.DaysOfWeek) > 0 {
		wd := int(now.Weekday())
		found := false
		for _, d := range p.DaysOfWeek {
			if d == wd {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if p.WindowStart != "" && p.WindowEnd != "" {
		cur := now.Format("15:04")
		// Same-day window (e.g. 16:00–18:00). Overnight windows not supported here.
		if cur < p.WindowStart || cur > p.WindowEnd {
			return false
		}
	}
	return true
}

// CreatePromotion creates a new promotion with proper defaults.
func (s *Service) CreatePromotion(ctx context.Context, tenantID uuid.UUID, name, promoCode string, startAt, endAt *time.Time, metadata map[string]any) (*ent.Promotion, error) {
	if promoCode == "" {
		promoCode = fmt.Sprintf("PROMO-%s", uuid.New().String()[:8])
	}

	builder := s.client.Promotion.Create().
		SetTenantID(tenantID).
		SetName(name).
		SetPromoCode(promoCode).
		SetStatus("active")

	if startAt != nil {
		builder = builder.SetStartAt(*startAt)
	}
	if endAt != nil {
		builder = builder.SetEndAt(*endAt)
	}
	if metadata != nil {
		builder = builder.SetMetadata(metadata)
	}

	return builder.Save(ctx)
}
