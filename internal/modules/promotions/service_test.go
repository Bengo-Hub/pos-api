package promotions

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/promotion"
	"github.com/bengobox/pos-service/internal/ent/promotionrule"
)

// autoApplyPromoKinds must include BOTH happy_hour (hospitality time-window deals) and auto
// (the generic "applies without a code" kind every other use case reaches for) — regression test
// for the "only hospitality discounts work" bug, where the query only ever loaded promo_kind=
// happy_hour and a retail/pharmacy/quick_service/services tenant's "Automatic" discount silently
// never fired at checkout. Must NOT include "code" (that only applies via ApplyPromoCode).
func TestAutoApplyPromoKinds(t *testing.T) {
	kinds := autoApplyPromoKinds()
	want := map[promotion.PromoKind]bool{promotion.PromoKindHappyHour: true, promotion.PromoKindAuto: true}
	if len(kinds) != len(want) {
		t.Fatalf("expected exactly %d auto-apply kinds, got %v", len(want), kinds)
	}
	for _, k := range kinds {
		if !want[k] {
			t.Errorf("unexpected auto-apply kind %q", k)
		}
		if k == promotion.PromoKindCode {
			t.Error("promo_kind=code must never be auto-evaluated — it only applies via ApplyPromoCode")
		}
	}
	for k := range want {
		found := false
		for _, got := range kinds {
			if got == k {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected auto-apply kinds to include %q, got %v", k, kinds)
		}
	}
}

// isWithinSchedule must treat a window whose start > end as crossing midnight (e.g. a bar happy
// hour 18:00–10:00) — the earlier code rejected these outright, so an overnight promo never fired.
func TestIsWithinSchedule_OvernightWindow(t *testing.T) {
	s := &Service{}
	p := &ent.Promotion{WindowStart: "18:00", WindowEnd: "10:00"} // overnight, no day restriction
	at := func(h, m int) time.Time { return time.Date(2026, 1, 2, h, m, 0, 0, time.UTC) }
	cases := []struct {
		h, m int
		want bool
	}{
		{20, 0, true},  // evening — inside
		{23, 59, true}, // just before midnight — inside
		{0, 30, true},  // after midnight — inside
		{9, 59, true},  // just before end — inside
		{10, 0, true},  // exactly end — inside (inclusive)
		{10, 1, false}, // just after end — outside
		{13, 0, false}, // mid-afternoon — outside
		{17, 59, false},// just before start — outside
		{18, 0, true},  // exactly start — inside (inclusive)
	}
	for _, c := range cases {
		if got := s.isWithinSchedule(p, at(c.h, c.m)); got != c.want {
			t.Errorf("overnight 18:00-10:00 at %02d:%02d = %v, want %v", c.h, c.m, got, c.want)
		}
	}
}

// A normal same-day window still behaves as before (start <= cur <= end).
func TestIsWithinSchedule_SameDayWindow(t *testing.T) {
	s := &Service{}
	p := &ent.Promotion{WindowStart: "16:00", WindowEnd: "18:00"}
	at := func(h, m int) time.Time { return time.Date(2026, 1, 2, h, m, 0, 0, time.UTC) }
	if !s.isWithinSchedule(p, at(17, 0)) {
		t.Error("17:00 should be inside 16:00-18:00")
	}
	if s.isWithinSchedule(p, at(12, 0)) {
		t.Error("12:00 should be outside 16:00-18:00")
	}
	if s.isWithinSchedule(p, at(20, 0)) {
		t.Error("20:00 should be outside 16:00-18:00")
	}
}

// Date-range boundary: a promo's start_at/end_at are inclusive at both ends, exclusive just
// outside — previously untested (only the daily time window had boundary coverage).
func TestIsWithinSchedule_DateRangeBoundary(t *testing.T) {
	s := &Service{}
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 30, 23, 59, 59, 0, time.UTC)
	p := &ent.Promotion{StartAt: start, EndAt: &end}
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before start", start.Add(-time.Second), false},
		{"exactly start", start, true},
		{"mid-range", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC), true},
		{"exactly end", end, true},
		{"after end", end.Add(time.Second), false},
	}
	for _, c := range cases {
		if got := s.isWithinSchedule(p, c.at); got != c.want {
			t.Errorf("%s: isWithinSchedule = %v, want %v", c.name, got, c.want)
		}
	}
}

// A no-EndAt promo (nil EndAt = no upper bound) is active indefinitely once started.
func TestIsWithinSchedule_NoEndDate(t *testing.T) {
	s := &Service{}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := &ent.Promotion{StartAt: start}
	if !s.isWithinSchedule(p, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("nil EndAt should never expire the promotion")
	}
}

// days_of_week restricts a promo to specific weekdays regardless of the time-of-day window.
func TestIsWithinSchedule_DaysOfWeek(t *testing.T) {
	s := &Service{}
	// 2026-01-05 is a Monday (weekday=1); restrict to Mon/Wed/Fri.
	p := &ent.Promotion{DaysOfWeek: []int{1, 3, 5}}
	mon := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	tue := time.Date(2026, 1, 6, 12, 0, 0, 0, time.UTC)
	wed := time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC)
	if !s.isWithinSchedule(p, mon) {
		t.Error("Monday should be within days_of_week=[Mon,Wed,Fri]")
	}
	if s.isWithinSchedule(p, tue) {
		t.Error("Tuesday should be outside days_of_week=[Mon,Wed,Fri]")
	}
	if !s.isWithinSchedule(p, wed) {
		t.Error("Wednesday should be within days_of_week=[Mon,Wed,Fri]")
	}
}

// An empty days_of_week means every day is allowed (no restriction).
func TestIsWithinSchedule_EmptyDaysOfWeekMeansEveryDay(t *testing.T) {
	s := &Service{}
	p := &ent.Promotion{}
	for d := 0; d < 7; d++ {
		at := time.Date(2026, 1, 4+d, 12, 0, 0, 0, time.UTC) // 2026-01-04 is a Sunday
		if !s.isWithinSchedule(p, at) {
			t.Errorf("day offset %d should be within an unrestricted schedule", d)
		}
	}
}

// scope_type=category matches the order line's Category field (case/whitespace-insensitive);
// only in-scope lines contribute to the base, and the discount is attributed proportionally
// across the scoped SKUs.
func TestEvaluateRule_CategoryScope(t *testing.T) {
	s := &Service{}
	rule := &ent.PromotionRule{
		DiscountType: promotionrule.DiscountTypePercentage,
		ScopeType:    promotionrule.ScopeTypeCategory,
		ScopeIds:     []string{"Beverages"},
		DiscountValue: 10,
	}
	lines := []DiscountLine{
		{SKU: "COKE", Category: " beverages ", Total: decimal.NewFromInt(1000)}, // in scope (trim/fold)
		{SKU: "BURGER", Category: "Food", Total: decimal.NewFromInt(2000)},      // out of scope
	}
	total, perSKU := s.evaluateRule(rule, lines, decimal.NewFromInt(3000))
	if !total.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("expected 10%% of the 1000 in-scope base = 100, got %s", total)
	}
	if _, ok := perSKU["BURGER"]; ok {
		t.Fatalf("out-of-category line must not be discounted: %+v", perSKU)
	}
	if perSKU["COKE"].Amount.IntPart() != 100 {
		t.Fatalf("expected COKE to carry the full 100, got %+v", perSKU["COKE"])
	}
}

// isWithinMealPeriod: the bug this closes — a rule tagged e.g. "lunch" previously fired at any
// hour because nothing ever read PromotionRule.MealPeriod. A rule with no meal_period set always
// passes (most discounts don't gate on it); one that IS set only passes inside its canonical
// window, inclusive at both boundaries like isWithinSchedule's own time-of-day check.
func TestIsWithinMealPeriod(t *testing.T) {
	lunch := promotionrule.MealPeriodLunch
	rule := &ent.PromotionRule{MealPeriod: &lunch}
	at := func(h, m int) time.Time { return time.Date(2026, 1, 2, h, m, 0, 0, time.UTC) }
	cases := []struct {
		h, m int
		want bool
	}{
		{11, 29, false}, // just before lunch window
		{11, 30, true},  // exactly start
		{13, 0, true},   // mid-lunch
		{15, 0, true},   // exactly end
		{15, 1, false},  // just after end
		{20, 0, false},  // dinner time
	}
	for _, c := range cases {
		if got := isWithinMealPeriod(rule, at(c.h, c.m)); got != c.want {
			t.Errorf("lunch rule at %02d:%02d = %v, want %v", c.h, c.m, got, c.want)
		}
	}
	if !isWithinMealPeriod(&ent.PromotionRule{}, at(3, 0)) {
		t.Error("a rule with no meal_period set must always pass, any hour")
	}
	if !isWithinMealPeriod(nil, at(3, 0)) {
		t.Error("a nil rule must pass (no gate to apply)")
	}
}

// line is a small helper to build a DiscountLine with a uniform per-unit price.
func line(sku string, qty float64, unit float64) DiscountLine {
	u := decimal.NewFromFloat(unit)
	return DiscountLine{SKU: sku, Quantity: qty, UnitPrice: u, Total: u.Mul(decimal.NewFromFloat(qty))}
}

func pairRule() *ent.PromotionRule {
	return &ent.PromotionRule{
		DiscountType:       promotionrule.DiscountTypeBogo,
		ScopeType:          promotionrule.ScopeTypeItem,
		ScopeIds:           []string{"PIZ003"},
		GetPairMap:         map[string]string{"PIZ003": "PIZ001"},
		BuyQuantity:        1,
		GetQuantity:        1,
		GetDiscountPercent: 100,
	}
}

// A bought Large + its corresponding Small in the cart frees exactly the mapped Small.
func TestCorrespondingPairBOGO_FreesMappedSmall(t *testing.T) {
	s := &Service{}
	lines := []DiscountLine{
		line("PIZ003", 1, 1200), // Margherita Large (buy)
		line("PIZ001", 1, 600),  // Margherita Small (its mapped free)
	}
	total, perSKU := s.calculateBOGODiscount(pairRule(), lines)
	if !total.Equal(decimal.NewFromInt(600)) {
		t.Fatalf("expected 600 discount (free small), got %s", total)
	}
	got, ok := perSKU["PIZ001"]
	if !ok || got.FreeQty != 1 {
		t.Fatalf("expected 1 free PIZ001, got %+v (ok=%v)", got, ok)
	}
}

// The freed unit must be the CORRESPONDING mapped Small — never a cheaper, unmapped one.
func TestCorrespondingPairBOGO_IgnoresCheaperUnmappedSmall(t *testing.T) {
	s := &Service{}
	lines := []DiscountLine{
		line("PIZ003", 1, 1200), // Margherita Large (buy)
		line("PIZ004", 1, 500),  // Pepperoni Small — cheaper, NOT mapped
		line("PIZ001", 1, 600),  // Margherita Small — the mapped free
	}
	total, perSKU := s.calculateBOGODiscount(pairRule(), lines)
	if !total.Equal(decimal.NewFromInt(600)) {
		t.Fatalf("expected the 600 mapped small to be freed (not the 500 unmapped), got %s", total)
	}
	if _, ok := perSKU["PIZ004"]; ok {
		t.Fatalf("unmapped cheaper small PIZ004 must not be discounted: %+v", perSKU)
	}
	if perSKU["PIZ001"].FreeQty != 1 {
		t.Fatalf("expected mapped PIZ001 freed once, got %+v", perSKU["PIZ001"])
	}
}

// No corresponding Small in the cart → nothing to free yet (the terminal auto-adds it separately).
func TestCorrespondingPairBOGO_NoMappedSmallNoDiscount(t *testing.T) {
	s := &Service{}
	lines := []DiscountLine{line("PIZ003", 1, 1200)}
	total, _ := s.calculateBOGODiscount(pairRule(), lines)
	if !total.IsZero() {
		t.Fatalf("expected 0 discount with no mapped small present, got %s", total)
	}
}

// Two Larges of the same flavor free two of their mapped Small (capped by cart availability).
func TestCorrespondingPairBOGO_ScalesWithBuyQty(t *testing.T) {
	s := &Service{}
	lines := []DiscountLine{
		line("PIZ003", 2, 1200),
		line("PIZ001", 2, 600),
	}
	total, perSKU := s.calculateBOGODiscount(pairRule(), lines)
	if !total.Equal(decimal.NewFromInt(1200)) {
		t.Fatalf("expected 1200 (two frees), got %s", total)
	}
	if perSKU["PIZ001"].FreeQty != 2 {
		t.Fatalf("expected 2 free PIZ001, got %+v", perSKU["PIZ001"])
	}
}

// burgerDayRuleWithSelfEntry reproduces the exact corrupted get_pair_map found on the Urban Loft
// "BURGER DAY" promotion in production (2026-08-15): five legitimate burger -> Urban Vege Burger
// pairs, plus a stray self-referencing "BUR005":"BUR005" entry that never appeared as a 6th row in
// the discount-form UI (which renders one row per map entry — so this key must have been written
// by a path other than the current pair editor) but WAS evaluated by calculateCorrespondingPairBOGO,
// earning every Urban Vege Burger sold a free credit for itself and zeroing its own price.
func burgerDayRuleWithSelfEntry() *ent.PromotionRule {
	return &ent.PromotionRule{
		DiscountType: promotionrule.DiscountTypeBogo,
		ScopeType:    promotionrule.ScopeTypeItem,
		ScopeIds:     []string{"BUR001", "BUR002", "BUR003", "BUR004", "BUR005", "BUR006"},
		GetPairMap: map[string]string{
			"BUR001": "BUR005",
			"BUR002": "BUR005",
			"BUR003": "BUR005",
			"BUR004": "BUR005",
			"BUR005": "BUR005", // the corrupted self-pair
			"BUR006": "BUR005",
		},
		BuyQuantity:        1,
		GetQuantity:        1,
		GetDiscountPercent: 100,
	}
}

// Buying ONLY the free item (Urban Vege Burger), with none of the qualifying burgers in the
// cart, must never discount it — the self-referencing entry must be ignored entirely.
func TestCorrespondingPairBOGO_SelfPairedEntryIgnored_SoloGetItemNeverFree(t *testing.T) {
	s := &Service{}
	lines := []DiscountLine{line("BUR005", 3, 450)} // 3 Urban Vege Burgers, nothing else bought
	total, perSKU := s.calculateBOGODiscount(burgerDayRuleWithSelfEntry(), lines)
	if !total.IsZero() {
		t.Fatalf("Urban Vege Burger bought alone must never self-discount, got %s (perSKU=%+v)", total, perSKU)
	}
}

// A legitimate pair (buy a qualifying burger, get the vege burger free) still works correctly
// even with the stray self-entry present in the same map.
func TestCorrespondingPairBOGO_SelfPairedEntryIgnored_LegitPairStillFrees(t *testing.T) {
	s := &Service{}
	lines := []DiscountLine{
		line("BUR001", 1, 550), // Beef Burger Bacon-No Cheese (buy)
		line("BUR005", 1, 450), // Urban Vege Burger (its mapped free)
	}
	total, perSKU := s.calculateBOGODiscount(burgerDayRuleWithSelfEntry(), lines)
	if !total.Equal(decimal.NewFromInt(450)) {
		t.Fatalf("expected 450 discount (free vege burger), got %s", total)
	}
	if got := perSKU["BUR005"]; got.FreeQty != 1 {
		t.Fatalf("expected exactly 1 free BUR005, got %+v", got)
	}
}

// A qualifying burger + TWO vege burgers must free exactly ONE (the earned credit), not both —
// the self-entry must not grant the second vege burger a free credit of its own.
func TestCorrespondingPairBOGO_SelfPairedEntryIgnored_DoesNotOverFree(t *testing.T) {
	s := &Service{}
	lines := []DiscountLine{
		line("BUR001", 1, 550),
		line("BUR005", 2, 450),
	}
	total, perSKU := s.calculateBOGODiscount(burgerDayRuleWithSelfEntry(), lines)
	if !total.Equal(decimal.NewFromInt(450)) {
		t.Fatalf("expected only 1 free vege burger (450), got %s", total)
	}
	if got := perSKU["BUR005"]; got.FreeQty != 1 {
		t.Fatalf("expected FreeQty=1 (not 2), got %+v", got)
	}
}
