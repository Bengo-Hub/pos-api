package handlers

import (
	"math"
	"testing"

	"github.com/bengobox/pos-service/internal/ent"
)

// A happy hour deal of 100 on the beer line stays on that line: the bill of 1000 (beer 400,
// food 600) is paid 900, so beer earns 300 and food keeps its 600.
func TestAttributeOrderLinesKeepsLineDiscountOnItsLine(t *testing.T) {
	o := &ent.POSOrder{TotalAmount: 900, DiscountTotal: 100}
	o.Edges.Lines = []*ent.POSOrderLine{
		{Sku: "BEER", Quantity: 2, TotalPrice: 400, Metadata: map[string]any{"discount_amount": 100.0}},
		{Sku: "FOOD", Quantity: 1, TotalPrice: 600},
	}
	got := AttributeOrderLines(o)
	if math.Abs(got[0].Revenue-300) > 0.001 || math.Abs(got[1].Revenue-600) > 0.001 {
		t.Fatalf("got beer %.2f food %.2f, want 300 and 600", got[0].Revenue, got[1].Revenue)
	}
}
