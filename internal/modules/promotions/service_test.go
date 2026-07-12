package promotions

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/promotionrule"
)

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
