package catalog

import (
	"testing"
	"time"
)

func TestUnitCostsBySKU(t *testing.T) {
	// 3 old units at 50 (FIFO) sold as one line of 3: unit cost 50, not the new standard 80.
	payload := map[string]any{"lines": []any{
		map[string]any{"sku": "X", "total_cost": 150.0},
		map[string]any{"sku": "Y", "total_cost": 90.0},
		map[string]any{"sku": "", "total_cost": 1.0},    // dropped
		map[string]any{"sku": "Z", "total_cost": "bad"}, // dropped
	}}
	lines := parseCostedLines(payload)
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	got := unitCostsBySKU(lines, map[string]float64{"X": 3, "Y": 4})
	if got["X"] != 50 || got["Y"] != 22.5 {
		t.Fatalf("unexpected unit costs: %v", got)
	}
	if _, ok := unitCostsBySKU(lines, map[string]float64{"X": 0})["X"]; ok {
		t.Fatal("zero quantity must not divide")
	}
}

func TestRetryDelayForBounded(t *testing.T) {
	for n := uint64(1); n <= 30; n++ {
		if d := retryDelayFor(n); d < 2*time.Second || d > 145*time.Second {
			t.Fatalf("delivery %d delay %s out of bounds", n, d)
		}
	}
}
