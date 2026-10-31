package orders

import "testing"

func TestOrderDiscountVATNoDiscount(t *testing.T) {
	if _, ok := OrderDiscountVAT([]SaleLineVATInput{{TotalPrice: 100, TaxAmount: 13.79, TaxRate: 16, Inclusive: true}}, 0); ok {
		t.Fatal("no discount must leave the recorded figures alone")
	}
}

// KES 100 off a bill of a 500 standard line and a 300 non-VAT line: shares 62.50 and 37.50, the
// standard line's VAT falls to 60.34, the bill falls by exactly 100.
func TestOrderDiscountVATSharesBeforeVAT(t *testing.T) {
	out, ok := OrderDiscountVAT([]SaleLineVATInput{
		{TotalPrice: 500, TaxAmount: 68.97, TaxRate: 16, Inclusive: true},
		{TotalPrice: 300, Inclusive: true},
	}, 100)
	if !ok {
		t.Fatal("expected the discount to be shared")
	}
	if out[0].PaidGross != 437.5 || out[0].Tax != 60.34 {
		t.Fatalf("standard line %+v, want 437.50 and 60.34", out[0])
	}
	if out[1].PaidGross != 262.5 || out[1].Tax != 0 {
		t.Fatalf("non-VAT line %+v, want 262.50 and no VAT", out[1])
	}
	if out[0].PaidGross+out[1].PaidGross != 700 {
		t.Fatalf("the bill must fall by exactly the discount")
	}
}

// An exclusive line stores the net price; its gross includes the VAT on top.
func TestOrderDiscountVATExclusiveLine(t *testing.T) {
	out, _ := OrderDiscountVAT([]SaleLineVATInput{
		{TotalPrice: 100, TaxAmount: 16, TaxRate: 16},
		{TotalPrice: 50, TaxAmount: 6.9, TaxRate: 16, Inclusive: true},
	}, 16.6)
	if out[0].PaidGross != 104.4 || out[0].Tax != 14.4 {
		t.Fatalf("exclusive line %+v, want 104.40 and 14.40", out[0])
	}
	if out[1].PaidGross != 45 || out[1].Tax != 6.21 {
		t.Fatalf("inclusive line %+v, want 45 and 6.21", out[1])
	}
}

// A line with no recorded rate uses the rate its own figures carry.
func TestOrderDiscountVATDerivedRate(t *testing.T) {
	out, _ := OrderDiscountVAT([]SaleLineVATInput{{TotalPrice: 116, TaxAmount: 16, Inclusive: true}}, 11.6)
	if out[0].PaidGross != 104.4 || out[0].Tax != 14.4 {
		t.Fatalf("got %+v", out[0])
	}
}
