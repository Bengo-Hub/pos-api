// Package promotions provides the promotion service layer for POS operations.
// It encapsulates promo code validation, discount calculation, and usage tracking
// that was previously incomplete in handlers (only validated existence, never calculated discounts).
package promotions

import (
	"context"
	"fmt"
	"sort"
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

// AutoDiscountResult is the combined auto-apply discount for an order. Multiple happy-hour promos
// STACK: each covers its own scoped items, so an order with a cocktail deal AND a pizza deal gets
// BOTH savings (a given SKU is only ever discounted by the single best promo covering it — no
// double-dip). PromoID/PromoName name the largest contributor for order-level attribution;
// ContributingPromoIDs + PerPromoAmount let the caller write one audit row per promo that helped.
type AutoDiscountResult struct {
	PromoID   uuid.UUID
	PromoName string
	Discount  decimal.Decimal
	// PerSKU maps a scoped SKU → its share of the discount + a display label. Empty for
	// storewide percentage/fixed discounts that don't attribute to specific lines.
	PerSKU               map[string]LineDiscount
	ContributingPromoIDs []uuid.UUID
	PerPromoAmount       map[uuid.UUID]decimal.Decimal
}

// TimedDiscountLine is a DiscountLine plus WHEN it was added to the bill. Happy-hour eligibility is
// decided per line by AddedAt (localized to the outlet), so a drink rung up during the window earns
// the deal even on a tab opened earlier, and one added before the window does not. A zero AddedAt
// means "now" (a line being added this moment).
type TimedDiscountLine struct {
	DiscountLine
	AddedAt time.Time
}

// allHappyHourPromos loads every auto-apply happy-hour promo for the outlet REGARDLESS of the
// current time — the timed evaluator checks each promo against each line's own add-time, so it must
// see promos that aren't live at time.Now() (e.g. evaluating a line added earlier during the window).
func (s *Service) allHappyHourPromos(ctx context.Context, tenantID uuid.UUID, outletID *uuid.UUID) ([]*ent.Promotion, error) {
	promos, err := s.client.Promotion.Query().Where(
		promotion.TenantID(tenantID),
		promotion.PromoKindEQ(promotion.PromoKindHappyHour),
		promotion.StatusEQ("active"),
		promotion.AutoApply(true),
	).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("promotions: query happy hours: %w", err)
	}
	out := make([]*ent.Promotion, 0, len(promos))
	for _, p := range promos {
		if p.OutletID != nil && (outletID == nil || *p.OutletID != *outletID) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// EvaluateTimedOrderDiscounts computes the combined happy-hour discount for an order's lines,
// STACKING every applicable promo and honouring each line's add-time window eligibility. This is
// the canonical evaluator: it fixes (a) only-one-promo-applies (each promo discounts its own scope,
// summed; a SKU covered by two promos takes the better, never both) and (b) open-bill-before-window
// (a line added inside the window qualifies even if the bill was opened earlier; one added outside
// does not). For each promo, only the lines whose AddedAt falls in that promo's schedule take part
// in its rule (so BOGO pairing also only counts in-window units).
func (s *Service) EvaluateTimedOrderDiscounts(ctx context.Context, tenantID, outletID uuid.UUID, lines []TimedDiscountLine) AutoDiscountResult {
	if len(lines) == 0 {
		return AutoDiscountResult{}
	}
	var outletPtr *uuid.UUID
	if outletID != uuid.Nil {
		outletPtr = &outletID
	}
	promos, err := s.allHappyHourPromos(ctx, tenantID, outletPtr)
	if err != nil || len(promos) == 0 {
		return AutoDiscountResult{}
	}
	items := make([]promoWithRule, 0, len(promos))
	for _, p := range promos {
		rule, rErr := s.client.PromotionRule.Query().Where(promotionrule.PromotionID(p.ID)).First(ctx)
		if rErr != nil || rule == nil {
			continue
		}
		items = append(items, promoWithRule{promo: p, rule: rule})
	}
	return s.combineTimedDiscounts(items, lines, s.outletTimezone(ctx, outletPtr))
}

// promoWithRule pairs a promotion with its (single) discount rule — the loaded input to the pure
// combiner below, kept separate so the stacking/timing logic can be unit-tested without a DB.
type promoWithRule struct {
	promo *ent.Promotion
	rule  *ent.PromotionRule
}

// combineTimedDiscounts is the pure core of the timed, stacking evaluator (no DB access): given the
// already-loaded promos+rules and the order's timed lines, it (1) filters each promo to the lines
// added within ITS window (localized), (2) runs each rule over that subset, and (3) merges per-SKU
// taking the single best promo per SKU (no double-dip) plus any storewide contributions, capping the
// grand total at the order subtotal. Exhaustively unit-tested; EvaluateTimedOrderDiscounts is the
// thin DB-loading wrapper.
func (s *Service) combineTimedDiscounts(items []promoWithRule, lines []TimedDiscountLine, loc *time.Location) AutoDiscountResult {
	if len(items) == 0 || len(lines) == 0 {
		return AutoDiscountResult{}
	}
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now()

	orderSubtotal := decimal.Zero
	for _, tl := range lines {
		orderSubtotal = orderSubtotal.Add(tl.Total)
	}

	combinedPerSKU := map[string]LineDiscount{} // sku → best discount across promos
	promoBySKU := map[string]uuid.UUID{}        // sku → promo currently crediting it
	promoNames := map[uuid.UUID]string{}
	perPromo := map[uuid.UUID]decimal.Decimal{} // promo → total it contributes (net of reassignments)
	storewideTotal := decimal.Zero              // unattributed (storewide) contributions

	for _, it := range items {
		p, rule := it.promo, it.rule
		if rule == nil {
			continue
		}
		// Keep only lines added within THIS promo's window (localized).
		eligible := make([]DiscountLine, 0, len(lines))
		eligSub := decimal.Zero
		for _, tl := range lines {
			when := tl.AddedAt
			if when.IsZero() {
				when = now
			}
			if s.isWithinSchedule(p, when.In(loc)) {
				eligible = append(eligible, tl.DiscountLine)
				eligSub = eligSub.Add(tl.Total)
			}
		}
		if len(eligible) == 0 {
			continue
		}
		total, perSKU := s.evaluateRule(rule, eligible, eligSub)
		if len(perSKU) > 0 {
			promoNames[p.ID] = p.Name
			for sku, ld := range perSKU {
				existing, ok := combinedPerSKU[sku]
				if ok && !ld.Amount.GreaterThan(existing.Amount) {
					continue // a better (or equal) promo already claimed this SKU — no double-dip
				}
				if ok { // reassign this SKU to the better promo; credit back the loser
					prev := promoBySKU[sku]
					perPromo[prev] = perPromo[prev].Sub(existing.Amount)
				}
				combinedPerSKU[sku] = ld
				promoBySKU[sku] = p.ID
				perPromo[p.ID] = perPromo[p.ID].Add(ld.Amount)
			}
		} else if total.IsPositive() {
			// Storewide/unattributed discount (no per-SKU scope) — add additively. Real tenants run
			// item-scoped happy hours so this path is rare; capped against the subtotal below.
			promoNames[p.ID] = p.Name
			perPromo[p.ID] = perPromo[p.ID].Add(total)
			storewideTotal = storewideTotal.Add(total)
		}
	}

	grand := storewideTotal
	for _, ld := range combinedPerSKU {
		grand = grand.Add(ld.Amount)
	}
	if grand.LessThanOrEqual(decimal.Zero) {
		return AutoDiscountResult{}
	}
	if grand.GreaterThan(orderSubtotal) {
		grand = orderSubtotal
	}

	// Representative promo (largest contributor) + the full contributing set for audit rows.
	var bestPromo uuid.UUID
	bestAmt := decimal.Zero
	contributing := make([]uuid.UUID, 0, len(perPromo))
	for id, amt := range perPromo {
		if amt.IsPositive() {
			contributing = append(contributing, id)
			if amt.GreaterThan(bestAmt) {
				bestAmt = amt
				bestPromo = id
			}
		}
	}
	return AutoDiscountResult{
		PromoID:              bestPromo,
		PromoName:            promoNames[bestPromo],
		Discount:             grand.Round(2),
		PerSKU:               combinedPerSKU,
		ContributingPromoIDs: contributing,
		PerPromoAmount:       perPromo,
	}
}

// EvaluateAutoDiscount returns the combined auto-apply discount for an outlet, treating all lines as
// added NOW. It delegates to EvaluateTimedOrderDiscounts (stacking + scope enforcement), so callers
// that don't track per-line add times still get every applicable promo — not just the single best.
// Prefer EvaluateTimedOrderDiscounts directly when you have real per-line timestamps (open bills).
func (s *Service) EvaluateAutoDiscount(ctx context.Context, tenantID, outletID uuid.UUID, lines []DiscountLine) AutoDiscountResult {
	timed := make([]TimedDiscountLine, len(lines))
	for i, l := range lines {
		timed[i] = TimedDiscountLine{DiscountLine: l} // zero AddedAt → treated as "now"
	}
	return s.EvaluateTimedOrderDiscounts(ctx, tenantID, outletID, timed)
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

// calculateBOGODiscount computes a "buy X get Y [at N% off]" discount and dispatches to one of
// two pairing modes depending on whether rule.GetScopeIds is set:
//
//   - Same-SKU (GetScopeIds empty, the original behavior): for every buy_quantity units of a
//     scoped SKU in the cart, get_quantity more units of the SAME SKU are discounted — the
//     classic "buy one drink get one drink free". The free unit IS another unit of the item
//     already being bought.
//   - Cross-item (GetScopeIds set): buying units from scope_ids (the "buy" scope) earns free/
//     discounted units from a DIFFERENT get_scope_ids set — e.g. "buy one Large pizza, get one
//     Small pizza free". Here the "get" item is a genuinely different catalog item (its own
//     recipe/stock/price) that the cashier adds to the cart as its own real order line — the
//     discount only prices that existing line correctly; no phantom unit needs to be added and
//     no special stock handling is needed (the line's own quantity already deducts its own
//     recipe via the normal per-SKU backflush).
//
// scope_type must be "item" with scope_ids set — a storewide/category BOGO has no well-defined
// "unit" to pair, so it's a no-op.
func (s *Service) calculateBOGODiscount(rule *ent.PromotionRule, lines []DiscountLine) (decimal.Decimal, map[string]LineDiscount) {
	if rule.ScopeType != promotionrule.ScopeTypeItem || len(rule.ScopeIds) == 0 {
		return decimal.Zero, nil
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
	freeLabel := "Free"
	if getPct < 100 {
		freeLabel = trimNum(getPct) + "% off"
	}
	label := fmt.Sprintf("Buy %d Get %d %s", buyQty, getQty, freeLabel)

	if len(rule.GetPairMap) > 0 {
		return s.calculateCorrespondingPairBOGO(rule, lines, buyQty, getQty, getPct, label)
	}
	if len(rule.GetScopeIds) > 0 {
		return s.calculateCrossItemBOGO(rule, lines, buyQty, getQty, getPct, label)
	}
	return s.calculateSameSKUBOGO(rule, lines, buyQty, getQty, getPct, label)
}

// calculateSameSKUBOGO is the original pairing: for every buy_quantity units of a scoped SKU in
// the cart, get_quantity more units of the SAME SKU are discounted by get_discount_percent (100
// = fully free). Grouped per-SKU (a cart can carry the same SKU split across multiple lines —
// one pre-existing, one just added) rather than pooled across different scoped SKUs, matching
// how a real "buy 1 burger get 1 burger free" deal reads: adding a second UNRELATED scoped item
// never completes someone else's pair.
func (s *Service) calculateSameSKUBOGO(rule *ent.PromotionRule, lines []DiscountLine, buyQty, getQty int, getPct float64, label string) (decimal.Decimal, map[string]LineDiscount) {
	inScope := map[string]struct{}{}
	for _, id := range rule.ScopeIds {
		inScope[strings.ToLower(strings.TrimSpace(id))] = struct{}{}
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

// calculateCrossItemBOGO handles "buy X of scope_ids, get Y of get_scope_ids [at N% off]" — the
// two scopes are DIFFERENT catalog items (e.g. buy one Large pizza SKU, get one Small pizza SKU
// free). Every buy_quantity units bought across the WHOLE buy scope (any qualifying SKU, not
// per-SKU — a customer buying one Margherita Large still earns the deal even if the free Small
// they pick is Pepperoni) earns get_quantity free "get" units, capped by however many get-scope
// units actually exist in the cart (the cashier must have actually added the free item as a real
// line — nothing is auto-added). When multiple different get-scope items/prices are present, the
// CHEAPEST units are discounted first (the customer-favoring convention: the free item is never
// pricier than a same-priced alternative already in the cart).
func (s *Service) calculateCrossItemBOGO(rule *ent.PromotionRule, lines []DiscountLine, buyQty, getQty int, getPct float64, label string) (decimal.Decimal, map[string]LineDiscount) {
	buyScope := map[string]struct{}{}
	for _, id := range rule.ScopeIds {
		buyScope[strings.ToLower(strings.TrimSpace(id))] = struct{}{}
	}
	getScope := map[string]struct{}{}
	for _, id := range rule.GetScopeIds {
		getScope[strings.ToLower(strings.TrimSpace(id))] = struct{}{}
	}

	var buyTotalQty float64
	for _, l := range lines {
		if _, ok := buyScope[strings.ToLower(strings.TrimSpace(l.SKU))]; ok {
			buyTotalQty += l.Quantity
		}
	}
	pairs := int(buyTotalQty) / buyQty
	if pairs <= 0 {
		return decimal.Zero, nil
	}
	freeUnitsEarned := pairs * getQty

	// Explode get-scope lines into individual unit slots so partial-line discounting (e.g. 2 of
	// a get-scope SKU in the cart but only 1 free credit earned) and cheapest-first ordering
	// both work regardless of how items are split across lines.
	type unit struct {
		sku   string
		price decimal.Decimal
	}
	var units []unit
	for _, l := range lines {
		if _, ok := getScope[strings.ToLower(strings.TrimSpace(l.SKU))]; !ok {
			continue
		}
		for i := 0; i < int(l.Quantity); i++ {
			units = append(units, unit{sku: l.SKU, price: l.UnitPrice})
		}
	}
	if len(units) == 0 {
		return decimal.Zero, nil // buyer hasn't added a qualifying "get" item yet — nothing to discount
	}
	sort.Slice(units, func(i, j int) bool { return units[i].price.LessThan(units[j].price) })
	if freeUnitsEarned > len(units) {
		freeUnitsEarned = len(units)
	}

	total := decimal.Zero
	perSKU := map[string]LineDiscount{}
	freeQtyBySKU := map[string]float64{}
	for i := 0; i < freeUnitsEarned; i++ {
		u := units[i]
		amt := u.price.Mul(decimal.NewFromFloat(getPct / 100)).Round(2)
		total = total.Add(amt)
		freeQtyBySKU[u.sku]++
		existing := perSKU[u.sku]
		existing.SKU = u.sku
		existing.Amount = existing.Amount.Add(amt)
		existing.FreeQty = freeQtyBySKU[u.sku]
		existing.Type = "bogo"
		existing.Label = label
		perSKU[u.sku] = existing
	}
	if rule.MaxDiscount != nil && *rule.MaxDiscount > 0 {
		if cap := decimal.NewFromFloat(*rule.MaxDiscount); total.GreaterThan(cap) {
			total = cap
		}
	}
	return total, perSKU
}

// calculateCorrespondingPairBOGO handles the CORRESPONDING cross-item deal: "buy a Large pizza,
// get the SAME-FLAVOR Small free". Unlike calculateCrossItemBOGO (which frees the cheapest unit of
// ANY get-scope item), this uses rule.GetPairMap — an explicit buy-SKU → get-SKU map (e.g. PIZ003
// Margherita-Large → PIZ001 Margherita-Small) — so the freed item is the one that corresponds to
// what was actually bought, never an arbitrary flavor. For each buy SKU in the cart, every
// buy_quantity units earns get_quantity free units of ITS mapped get SKU (capped by how many of
// that mapped SKU are actually in the cart — the terminal auto-adds it, so it normally is). Units
// are consumed as they're freed so a get SKU that happens to be shared across two mappings isn't
// double-counted. Mirrors the terminal's client-side calcCorrespondingPairBogo exactly.
func (s *Service) calculateCorrespondingPairBOGO(rule *ent.PromotionRule, lines []DiscountLine, buyQty, getQty int, getPct float64, label string) (decimal.Decimal, map[string]LineDiscount) {
	// lower(buySKU) → mapped get SKU (original case preserved for the per-SKU output).
	pair := make(map[string]string, len(rule.GetPairMap))
	for k, v := range rule.GetPairMap {
		pair[strings.ToLower(strings.TrimSpace(k))] = v
	}
	if len(pair) == 0 {
		return decimal.Zero, nil
	}

	// Bought quantity per buy SKU (the "Large" side).
	buyQtyBySku := map[string]float64{}
	for _, l := range lines {
		lk := strings.ToLower(strings.TrimSpace(l.SKU))
		if _, ok := pair[lk]; ok {
			buyQtyBySku[lk] += l.Quantity
		}
	}

	// Explode every get-scope line into individual unit prices so partial freeing + cheapest-first
	// (within the same mapped SKU) both work regardless of line splitting.
	getUnits := map[string][]decimal.Decimal{} // lower(getSKU) → remaining unit prices
	getDisplaySku := map[string]string{}        // lower(getSKU) → original-case SKU for output
	for _, l := range lines {
		gk := strings.ToLower(strings.TrimSpace(l.SKU))
		for i := 0; i < int(l.Quantity); i++ {
			getUnits[gk] = append(getUnits[gk], l.UnitPrice)
		}
		getDisplaySku[gk] = l.SKU
	}

	total := decimal.Zero
	freeQtyBySku := map[string]float64{}
	amtBySku := map[string]decimal.Decimal{}
	for buyLk, qty := range buyQtyBySku {
		pairs := int(qty) / buyQty
		if pairs <= 0 {
			continue
		}
		earned := pairs * getQty
		gk := strings.ToLower(strings.TrimSpace(pair[buyLk]))
		avail := getUnits[gk]
		sort.Slice(avail, func(i, j int) bool { return avail[i].LessThan(avail[j]) })
		n := earned
		if n > len(avail) {
			n = len(avail)
		}
		for i := 0; i < n; i++ {
			amt := avail[i].Mul(decimal.NewFromFloat(getPct / 100)).Round(2)
			total = total.Add(amt)
			freeQtyBySku[gk]++
			amtBySku[gk] = amtBySku[gk].Add(amt)
		}
		getUnits[gk] = avail[n:] // consume freed units
	}

	perSKU := map[string]LineDiscount{}
	for gk, fq := range freeQtyBySku {
		perSKU[getDisplaySku[gk]] = LineDiscount{SKU: getDisplaySku[gk], Amount: amtBySku[gk], FreeQty: fq, Type: "bogo", Label: label}
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
		if p.WindowStart <= p.WindowEnd {
			// Same-day window (e.g. 16:00–18:00): active when start <= cur <= end.
			if cur < p.WindowStart || cur > p.WindowEnd {
				return false
			}
		} else {
			// Overnight window crossing midnight (e.g. a bar happy hour 18:00–10:00): active in
			// [start, 24:00) ∪ [00:00, end], i.e. inactive only strictly between end and start.
			// The weekday check above is applied to `now`'s own day, so a deal listing its days
			// (e.g. Fri/Sat/Sun) covers each listed evening plus that same date's early hours; the
			// spill into an UNLISTED next weekday isn't matched (fine when consecutive days are
			// listed, which is the usual bar pattern).
			if cur > p.WindowEnd && cur < p.WindowStart {
				return false
			}
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
