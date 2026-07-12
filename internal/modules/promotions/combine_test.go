package promotions

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/promotionrule"
)

// These tests lock the three live bugs found 2026-07-12 in the pure stacking/timing core
// (combineTimedDiscounts): (1) only-one-promo-applies, (2) items added to an already-open bill
// missing the deal, (3) their interaction on one docket — each decided per line by add-time.

func uid() uuid.UUID { return uuid.New() }

func promoW(name, ws, we string, days []int) *ent.Promotion {
	return &ent.Promotion{ID: uid(), Name: name, WindowStart: ws, WindowEnd: we, DaysOfWeek: days}
}

func sameSkuBogoRule(getPct float64, skus ...string) *ent.PromotionRule {
	return &ent.PromotionRule{
		DiscountType: promotionrule.DiscountTypeBogo, ScopeType: promotionrule.ScopeTypeItem,
		ScopeIds: skus, BuyQuantity: 1, GetQuantity: 1, GetDiscountPercent: getPct,
	}
}

func pairBogoRule(pairs map[string]string) *ent.PromotionRule {
	buy := make([]string, 0, len(pairs))
	get := make([]string, 0, len(pairs))
	for k, v := range pairs {
		buy = append(buy, k)
		get = append(get, v)
	}
	return &ent.PromotionRule{
		DiscountType: promotionrule.DiscountTypeBogo, ScopeType: promotionrule.ScopeTypeItem,
		ScopeIds: buy, GetScopeIds: get, GetPairMap: pairs,
		BuyQuantity: 1, GetQuantity: 1, GetDiscountPercent: 100,
	}
}

func tline(sku string, qty, unit float64, addedAt time.Time) TimedDiscountLine {
	u := decimal.NewFromFloat(unit)
	return TimedDiscountLine{
		DiscountLine: DiscountLine{SKU: sku, Quantity: qty, UnitPrice: u, Total: u.Mul(decimal.NewFromFloat(qty))},
		AddedAt:      addedAt,
	}
}

func atUTC(h, m int) time.Time { return time.Date(2026, 7, 12, h, m, 0, 0, time.UTC) }

// The exact receipt scenario: a cocktail BOGO AND a pizza corresponding-pair deal on ONE docket
// must BOTH apply. Before the fix only the single largest (cocktail, 1000) applied and the pizza
// free (900) was silently dropped.
func TestCombine_TwoPromosStackOnOneOrder(t *testing.T) {
	s := &Service{}
	items := []promoWithRule{
		{promo: promoW("HAPPY HOUR", "18:00", "10:00", nil), rule: sameSkuBogoRule(100, "COC007")},
		{promo: promoW("pizza Bogo", "07:30", "22:30", nil), rule: pairBogoRule(map[string]string{"PIZ009": "PIZ007"})},
	}
	lines := []TimedDiscountLine{
		tline("COC007", 2, 1000, atUTC(20, 0)), // BOGO → 1 free = 1000
		tline("PIZ009", 1, 1600, atUTC(20, 0)), // buy Large
		tline("PIZ007", 1, 900, atUTC(20, 0)),  // corresponding Small → free = 900
		tline("JUI002", 1, 400, atUTC(20, 0)),  // not in any promo
	}
	r := s.combineTimedDiscounts(items, lines, time.UTC)
	if !r.Discount.Equal(decimal.NewFromInt(1900)) {
		t.Fatalf("expected combined 1900 (1000 cocktail + 900 pizza), got %s", r.Discount)
	}
	if r.PerSKU["COC007"].Amount.IntPart() != 1000 || r.PerSKU["PIZ007"].Amount.IntPart() != 900 {
		t.Fatalf("expected per-SKU 1000/900, got %+v", r.PerSKU)
	}
	if len(r.ContributingPromoIDs) != 2 {
		t.Fatalf("expected 2 contributing promos, got %d", len(r.ContributingPromoIDs))
	}
}

// Two promos both covering the SAME SKU must NOT double-dip — only the better one counts.
func TestCombine_NoDoubleDipSameSku(t *testing.T) {
	s := &Service{}
	items := []promoWithRule{
		{promo: promoW("Half off", "00:00", "23:59", nil), rule: sameSkuBogoRule(50, "COC007")},   // free=500
		{promo: promoW("Full BOGO", "00:00", "23:59", nil), rule: sameSkuBogoRule(100, "COC007")}, // free=1000
	}
	lines := []TimedDiscountLine{tline("COC007", 2, 1000, atUTC(12, 0))}
	r := s.combineTimedDiscounts(items, lines, time.UTC)
	if !r.Discount.Equal(decimal.NewFromInt(1000)) {
		t.Fatalf("expected 1000 (best only, not 1500), got %s", r.Discount)
	}
}

// Open bill: a pizza pair whose BOTH items were added inside the window earns the deal.
func TestCombine_OpenBill_AddedDuringWindowQualifies(t *testing.T) {
	s := &Service{}
	items := []promoWithRule{{promo: promoW("pizza Bogo", "07:30", "22:30", nil), rule: pairBogoRule(map[string]string{"PIZ009": "PIZ007"})}}
	lines := []TimedDiscountLine{
		tline("PIZ009", 1, 1600, atUTC(8, 0)), // added 08:00, in window
		tline("PIZ007", 1, 900, atUTC(9, 0)),  // added 09:00, in window
	}
	r := s.combineTimedDiscounts(items, lines, time.UTC)
	if !r.Discount.Equal(decimal.NewFromInt(900)) {
		t.Fatalf("expected 900 for in-window pair, got %s", r.Discount)
	}
}

// The bug: a line added BEFORE the window must not earn the deal — here the Large was added at
// 06:00 (before 07:30), so the pair never forms and nothing is discounted.
func TestCombine_AddedBeforeWindowDoesNotQualify(t *testing.T) {
	s := &Service{}
	items := []promoWithRule{{promo: promoW("pizza Bogo", "07:30", "22:30", nil), rule: pairBogoRule(map[string]string{"PIZ009": "PIZ007"})}}
	lines := []TimedDiscountLine{
		tline("PIZ009", 1, 1600, atUTC(6, 0)), // BEFORE window
		tline("PIZ007", 1, 900, atUTC(9, 0)),  // in window, but its buy-partner is not eligible
	}
	r := s.combineTimedDiscounts(items, lines, time.UTC)
	if !r.Discount.IsZero() {
		t.Fatalf("expected 0 (buy item added before window), got %s", r.Discount)
	}
}

// Overnight window: a cocktail rung up at 20:00 qualifies; the same at 13:00 does not.
func TestCombine_OvernightWindowLineTiming(t *testing.T) {
	s := &Service{}
	items := []promoWithRule{{promo: promoW("HAPPY HOUR", "18:00", "10:00", nil), rule: sameSkuBogoRule(100, "COC007")}}
	inWindow := s.combineTimedDiscounts(items, []TimedDiscountLine{tline("COC007", 2, 1000, atUTC(20, 0))}, time.UTC)
	if !inWindow.Discount.Equal(decimal.NewFromInt(1000)) {
		t.Fatalf("20:00 should qualify overnight deal, got %s", inWindow.Discount)
	}
	outWindow := s.combineTimedDiscounts(items, []TimedDiscountLine{tline("COC007", 2, 1000, atUTC(13, 0))}, time.UTC)
	if !outWindow.Discount.IsZero() {
		t.Fatalf("13:00 must NOT qualify overnight deal, got %s", outWindow.Discount)
	}
}

// Two corresponding pizza pairs on one order each free their own matching Small.
func TestCombine_MultiplePizzaPairs(t *testing.T) {
	s := &Service{}
	items := []promoWithRule{{promo: promoW("pizza Bogo", "07:30", "22:30", nil),
		rule: pairBogoRule(map[string]string{"PIZ009": "PIZ007", "PIZ012": "PIZ010"})}}
	lines := []TimedDiscountLine{
		tline("PIZ009", 1, 1600, atUTC(10, 0)), tline("PIZ007", 1, 900, atUTC(10, 0)),
		tline("PIZ012", 1, 1800, atUTC(10, 0)), tline("PIZ010", 1, 1000, atUTC(10, 0)),
	}
	r := s.combineTimedDiscounts(items, lines, time.UTC)
	if !r.Discount.Equal(decimal.NewFromInt(1900)) { // 900 + 1000
		t.Fatalf("expected 1900 for two pairs, got %s", r.Discount)
	}
}

// A mixed bill where only SOME lines are in-window: pizza (day, in-window) applies, a cocktail added
// in the afternoon (before the 18:00 cocktail window) does not — the exact "some applied, some
// missed" pattern, now decided per line.
func TestCombine_MixedTiming(t *testing.T) {
	s := &Service{}
	items := []promoWithRule{
		{promo: promoW("HAPPY HOUR", "18:00", "10:00", nil), rule: sameSkuBogoRule(100, "COC007")},
		{promo: promoW("pizza Bogo", "07:30", "22:30", nil), rule: pairBogoRule(map[string]string{"PIZ009": "PIZ007"})},
	}
	lines := []TimedDiscountLine{
		tline("COC007", 2, 1000, atUTC(15, 0)), // 15:00 — BEFORE cocktail window → no discount
		tline("PIZ009", 1, 1600, atUTC(15, 0)), // 15:00 — inside pizza window
		tline("PIZ007", 1, 900, atUTC(15, 0)),  // 15:00 — inside pizza window → free 900
	}
	r := s.combineTimedDiscounts(items, lines, time.UTC)
	if !r.Discount.Equal(decimal.NewFromInt(900)) {
		t.Fatalf("expected only the pizza 900 (cocktail added pre-window), got %s", r.Discount)
	}
	if _, ok := r.PerSKU["COC007"]; ok {
		t.Fatalf("cocktail added before its window must not be discounted: %+v", r.PerSKU)
	}
}

// Empty inputs are safe no-ops.
func TestCombine_EmptyInputs(t *testing.T) {
	s := &Service{}
	if r := s.combineTimedDiscounts(nil, []TimedDiscountLine{tline("X", 1, 100, atUTC(12, 0))}, time.UTC); !r.Discount.IsZero() {
		t.Fatalf("no promos → 0, got %s", r.Discount)
	}
	items := []promoWithRule{{promo: promoW("p", "00:00", "23:59", nil), rule: sameSkuBogoRule(100, "COC007")}}
	if r := s.combineTimedDiscounts(items, nil, time.UTC); !r.Discount.IsZero() {
		t.Fatalf("no lines → 0, got %s", r.Discount)
	}
}
