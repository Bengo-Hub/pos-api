package orders

import (
	"math"
	"testing"

	"github.com/bengobox/pos-service/internal/ent"
)

// grossOf rebuilds the line total the way treasury does: quantity x price before tax x (1 + rate).
func grossOf(q, unit, rate float64) float64 { return q * unit * (1 + rate/100) }

func TestOrderDiscountRatio(t *testing.T) {
	lines := []*ent.POSOrderLine{{TotalPrice: 600}, {TotalPrice: 400}}
	if got := OrderDiscountRatio(1000, lines); got != 0 {
		t.Fatalf("no discount: got %v", got)
	}
	if got := OrderDiscountRatio(900, lines); math.Abs(got-0.1) > 1e-9 {
		t.Fatalf("10%% discount: got %v", got)
	}
	if got := OrderDiscountRatio(1200, lines); got != 0 {
		t.Fatalf("total above the gross must not give a negative ratio: got %v", got)
	}
	if got := OrderDiscountRatio(50, nil); got != 0 {
		t.Fatalf("no lines: got %v", got)
	}
	// An exclusive line's total_price is net; its VAT counts toward the gross, so an undiscounted
	// order with one is not mistaken for a discounted one (or the reverse).
	tax := 16.0
	mixed := []*ent.POSOrderLine{{TotalPrice: 100, TaxAmount: &tax, PriceIncludesTax: false}, {TotalPrice: 50, PriceIncludesTax: true}}
	if got := OrderDiscountRatio(166, mixed); got != 0 {
		t.Fatalf("undiscounted order with an exclusive line: got %v", got)
	}
	if got := OrderDiscountRatio(149.4, mixed); math.Abs(got-0.1) > 1e-9 {
		t.Fatalf("10%% off a mixed order: got %v", got)
	}
}

func TestBuildRefundLines(t *testing.T) {
	// 2 of 4 units returned, VAT inclusive 16%: gross 296.96 carries 40.96 tax.
	got := BuildRefundLines(0, []RefundLineInput{{SKU: "PHM-1", Name: "Paracetamol", Quantity: 2, Gross: 296.96, Tax: 40.96, TaxCode: "VAT-16"}})
	if len(got) != 1 || got[0].ItemSKU != "PHM-1" || got[0].TaxCode != "VAT-16" || got[0].Quantity != 2 {
		t.Fatalf("unexpected line: %+v", got)
	}
	if math.Abs(got[0].TaxRate-16) > 0.001 {
		t.Fatalf("rate: got %v want 16", got[0].TaxRate)
	}
	if g := grossOf(got[0].Quantity, got[0].UnitPrice, got[0].TaxRate); math.Abs(g-296.96) > 0.005 {
		t.Fatalf("treasury would rebuild gross %.4f, want 296.96", g)
	}

	// A 10% header discount shrinks gross and tax together, so the rate does not move.
	d := BuildRefundLines(0.1, []RefundLineInput{{Name: "Mug", Quantity: 1, Gross: 522, Tax: 72}})
	if math.Abs(d[0].TaxRate-16) > 0.001 {
		t.Fatalf("discounted rate moved: %v", d[0].TaxRate)
	}
	if g := grossOf(d[0].Quantity, d[0].UnitPrice, d[0].TaxRate); math.Abs(g-469.8) > 0.005 {
		t.Fatalf("discounted gross %.4f, want 469.80", g)
	}

	// No tax on the line: the sale carried no VAT, so the refund carries none (never an assumed 16%).
	f := BuildRefundLines(0, []RefundLineInput{{Name: "Water", Quantity: 3, Gross: 240}})
	if f[0].TaxRate != 0 {
		t.Fatalf("an untaxed line must refund no VAT: rate %v", f[0].TaxRate)
	}
	if g := grossOf(f[0].Quantity, f[0].UnitPrice, f[0].TaxRate); math.Abs(g-240) > 0.005 {
		t.Fatalf("untaxed gross %.4f, want 240", g)
	}

	// Lines with nothing coming back are dropped.
	if z := BuildRefundLines(0, []RefundLineInput{{Name: "x", Quantity: 0, Gross: 10}, {Name: "y", Quantity: 1, Gross: 0}}); len(z) != 0 {
		t.Fatalf("empty lines kept: %+v", z)
	}
}
