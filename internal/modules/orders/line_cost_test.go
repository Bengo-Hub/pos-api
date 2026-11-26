package orders

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

func TestLineUnitCostAtSale(t *testing.T) {
	cases := []struct {
		name string
		md   map[string]any
		want *float64
	}{
		{"no metadata", nil, nil},
		{"no key", map[string]any{"other": 1.0}, nil},
		{"till float", map[string]any{LineUnitCostAtSaleKey: 120.5}, costPtr(120.5)},
		{"int value", map[string]any{LineUnitCostAtSaleKey: 80}, costPtr(80)},
		{"inventory value", map[string]any{LineUnitCostAtSaleKey: 95.25, LineCostSourceKey: LineCostSourceInventory}, costPtr(95.25)},
		{"zero from inventory is real", map[string]any{LineUnitCostAtSaleKey: 0.0, LineCostSourceKey: LineCostSourceInventory}, costPtr(0)},
		{"bare zero is unknown", map[string]any{LineUnitCostAtSaleKey: 0.0}, nil},
		{"negative ignored", map[string]any{LineUnitCostAtSaleKey: -5.0}, nil},
		{"string ignored", map[string]any{LineUnitCostAtSaleKey: "12"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LineUnitCostAtSale(&ent.POSOrderLine{Metadata: tc.md})
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("want nil, got %v", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("want %v, got nil", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("want %v, got %v", *tc.want, *got)
			}
		})
	}
	if LineUnitCostAtSale(nil) != nil {
		t.Fatal("nil line must give nil")
	}
}

func TestResolveLineUnitCostsPrefersSnapshot(t *testing.T) {
	snap := &ent.POSOrderLine{ID: uuid.New(), Sku: "A", Metadata: map[string]any{LineUnitCostAtSaleKey: 50.0}}
	inv := &ent.POSOrderLine{ID: uuid.New(), Sku: "B", Metadata: map[string]any{LineUnitCostAtSaleKey: 0.0, LineCostSourceKey: LineCostSourceInventory}}
	none := &ent.POSOrderLine{ID: uuid.New(), Sku: "C"}
	unknown := &ent.POSOrderLine{ID: uuid.New(), Sku: "D"}

	// The catalog has since moved for A and B; the sale's own cost must still win for them.
	fallback := map[string]float64{"A": 999, "B": 999, "C": 30}
	got := resolveLineUnitCosts([]*ent.POSOrderLine{snap, inv, none, unknown, nil}, fallback)

	want := map[uuid.UUID]float64{snap.ID: 50, inv.ID: 0, none.ID: 30, unknown.ID: 0}
	if len(got) != len(want) {
		t.Fatalf("want %d entries, got %d: %v", len(want), len(got), got)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("line %s: want %v, got %v", id, w, got[id])
		}
	}
}

func TestBlendedUnitCost(t *testing.T) {
	if got := BlendedUnitCost(100, 2, 130, 1); got != 110 {
		t.Fatalf("want 110, got %v", got)
	}
	if got := BlendedUnitCost(0, 0, 40, 0); got != 40 {
		t.Fatalf("zero quantity should fall back to the added cost, got %v", got)
	}
}

func costPtr(f float64) *float64 { return &f }
