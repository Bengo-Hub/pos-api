package handlers

import "testing"

func TestLoyaltyTiers(t *testing.T) {
	tiers := loyaltyTiers(map[string]any{"gold": float64(2000), "bronze": float64(0), "silver": 500, "bad": "x"})
	if len(tiers) != 3 || tiers[0].Name != "bronze" || tiers[1].Name != "silver" || tiers[2].Name != "gold" {
		t.Fatalf("tiers = %+v", tiers)
	}
	for lifetime, want := range map[int]string{0: "bronze", 499: "bronze", 500: "silver", 5000: "gold"} {
		if got := loyaltyTierFor(tiers, lifetime); got != want {
			t.Fatalf("lifetime %d -> %q, want %q", lifetime, got, want)
		}
	}
	if got := loyaltyTierFor(loyaltyTiers(map[string]any{"silver": float64(100)}), 10); got != "" {
		t.Fatalf("below every level -> %q, want empty", got)
	}
}
