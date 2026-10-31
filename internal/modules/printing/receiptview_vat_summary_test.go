package printing

import (
	"testing"

	"github.com/bengobox/pos-service/internal/ent"
)

func f64(v float64) *float64 { return &v }

// The receipt's VAT block uses the lines as the sale event sends them: active quantity, VAT scaled
// to it, and the order discount shared before VAT.
func TestBuildReceiptViewVATSummary(t *testing.T) {
	order := &ent.POSOrder{OrderNumber: "000201", Currency: "KES", DiscountTotal: 100}
	lines := []*ent.POSOrderLine{
		// 2 of 3 still on the bill: 500 gross, 68.97 VAT.
		{Name: "Shampoo", Quantity: 3, UnitPrice: 250, TotalPrice: 750, TaxRate: f64(16), TaxAmount: f64(103.455), PriceIncludesTax: true, VoidedQty: f64(1)},
		{Name: "Bread", Quantity: 1, UnitPrice: 300, TotalPrice: 300, PriceIncludesTax: true},
		{Name: "Voided", Quantity: 1, UnitPrice: 50, TotalPrice: 50, TaxRate: f64(16), TaxAmount: f64(6.9), PriceIncludesTax: true, VoidedQty: f64(1)},
	}
	v := BuildReceiptView(order, lines, nil, nil, ReceiptViewOpts{})
	if len(v.VatSummary) != 1 || v.VatSummary[0].Tax != 60.34 || v.VatSummary[0].Taxable != 377.16 {
		t.Fatalf("VatSummary %+v, want one 16%% row 377.16 / 60.34", v.VatSummary)
	}

	split := BuildReceiptView(order, lines, nil, nil, ReceiptViewOpts{SplitLineIDs: map[string]bool{}})
	if len(split.VatSummary) != 0 {
		t.Fatalf("a split bill carries no VAT summary, got %+v", split.VatSummary)
	}
}
