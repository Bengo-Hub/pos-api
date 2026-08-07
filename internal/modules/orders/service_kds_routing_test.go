package orders

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/kdsstation"
)

// 2026-08 urban-loft regression: a café's "Bar" station commonly owns hot drinks (Coffees/Teas)
// via an explicit category_filter — that tenant configuration must win over the generic
// "hot beverages belong in the kitchen" name-based guess. See resolveStationForLine.
func TestResolveStationForLine_CategoryFilterWinsOverHotBeverageGuess(t *testing.T) {
	kitchenID := uuid.New()
	barID := uuid.New()
	stations := []*ent.KDSStation{
		{ID: kitchenID, StationType: kdsstation.StationTypeKitchen, CategoryFilter: []string{"Main Dishes", "Salad"}},
		{ID: barID, StationType: kdsstation.StationTypeBar, CategoryFilter: []string{"Coffees", "Teas", "Milkshakes"}},
	}

	cases := []struct {
		name, itemName, category string
		want                     uuid.UUID
	}{
		{"tea by category", "Mixed Tea", "Teas", barID},
		{"hot water lemon by category", "Hot Water Lemon", "Teas", barID},
		{"coffee by category", "White Mocha Double", "Coffees", barID},
		{"cold mocha shake by category", "Mocha Shake", "Milkshakes", barID},
		{"kitchen food unaffected", "Grilled Chicken", "Main Dishes", kitchenID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveStationForLine(c.itemName, c.category, nil, stations)
			if got == nil || *got != c.want {
				t.Fatalf("resolveStationForLine(%q, %q) = %v, want %v", c.itemName, c.category, got, c.want)
			}
		})
	}
}

// Uncategorized legacy items still fall back to the hot-beverage-to-kitchen name guess when no
// station's category_filter matches anything at all.
func TestResolveStationForLine_HotBeverageFallbackOnlyWhenUnmatched(t *testing.T) {
	kitchenID := uuid.New()
	stations := []*ent.KDSStation{
		{ID: kitchenID, StationType: kdsstation.StationTypeKitchen, CategoryFilter: []string{"Main Dishes"}},
	}
	got := resolveStationForLine("Cappuccino", "", nil, stations)
	if got == nil || *got != kitchenID {
		t.Fatalf("expected uncategorized hot beverage to fall back to kitchen, got %v", got)
	}
}

// An explicit per-item override always wins, regardless of category_filter configuration.
func TestResolveStationForLine_ExplicitOverrideWins(t *testing.T) {
	kitchenID := uuid.New()
	barID := uuid.New()
	overrideID := uuid.New()
	stations := []*ent.KDSStation{
		{ID: kitchenID, StationType: kdsstation.StationTypeKitchen, CategoryFilter: []string{"Teas"}},
		{ID: barID, StationType: kdsstation.StationTypeBar, CategoryFilter: []string{}},
	}
	got := resolveStationForLine("Mixed Tea", "Teas", &overrideID, stations)
	if got == nil || *got != overrideID {
		t.Fatalf("expected explicit override to win, got %v", got)
	}
}
