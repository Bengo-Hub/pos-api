package saletax

import "testing"

func TestRateSummaryIncludesInclusiveVAT(t *testing.T) {
	rows := RateSummary([]SaleLineVATInput{
		{TotalPrice: 116, TaxAmount: 16, TaxRate: 16, Inclusive: true},
		{TotalPrice: 100, TaxAmount: 16, TaxRate: 16},
		{TotalPrice: 108, TaxAmount: 8, TaxRate: 8, Inclusive: true},
		{TotalPrice: 50, Inclusive: true},
	}, 0)
	if len(rows) != 2 {
		t.Fatalf("rows %+v, want 16%% and 8%%", rows)
	}
	if rows[0].Rate != 16 || rows[0].Taxable != 200 || rows[0].Tax != 32 {
		t.Fatalf("16%% row %+v, want taxable 200 VAT 32", rows[0])
	}
	if rows[1].Rate != 8 || rows[1].Taxable != 100 || rows[1].Tax != 8 {
		t.Fatalf("8%% row %+v", rows[1])
	}
}

// With an order discount the receipt shows the VAT on what was paid, the figure KRA receives.
func TestRateSummaryAfterOrderDiscount(t *testing.T) {
	rows := RateSummary([]SaleLineVATInput{
		{TotalPrice: 500, TaxAmount: 68.97, TaxRate: 16, Inclusive: true},
		{TotalPrice: 300, Inclusive: true},
	}, 100)
	if len(rows) != 1 || rows[0].Tax != 60.34 || rows[0].Taxable != 377.16 {
		t.Fatalf("rows %+v, want one 16%% row 377.16 / 60.34", rows)
	}
}

func TestRateSummaryNoVAT(t *testing.T) {
	if rows := RateSummary([]SaleLineVATInput{{TotalPrice: 300, Inclusive: true}}, 0); len(rows) != 0 {
		t.Fatalf("a sale with no VAT has no rows, got %+v", rows)
	}
}
