package payments

import (
	"testing"

	"github.com/bengobox/pos-service/internal/modules/treasury"
)

func TestShareOrderDiscountVAT(t *testing.T) {
	items := []map[string]any{
		{"total_price": 500.0, "tax_amount": 68.97, "tax_rate": 16.0, "price_includes_tax": true},
		{"total_price": 300.0, "price_includes_tax": true},
	}
	shareOrderDiscountVAT(items, nil, 100)
	if items[0]["paid_gross"] != 437.5 || items[0]["tax_amount"] != 60.34 {
		t.Fatalf("standard line %v", items[0])
	}
	if items[1]["paid_gross"] != 262.5 {
		t.Fatalf("non-VAT line %v", items[1])
	}
	if _, has := items[1]["tax_amount"]; has {
		t.Fatalf("a line with no VAT must not gain a tax_amount: %v", items[1])
	}

	plain := []map[string]any{{"total_price": 116.0, "tax_amount": 16.0, "tax_rate": 16.0, "price_includes_tax": true}}
	shareOrderDiscountVAT(plain, nil, 0)
	if _, has := plain[0]["paid_gross"]; has || plain[0]["tax_amount"] != 16.0 {
		t.Fatalf("an undiscounted sale must go out as recorded: %v", plain[0])
	}
}

// A happy hour discount of 50 on the B line stays on that line in both the sale event and the
// sync signing request; the exempt line goes out untouched.
func TestShareLineDiscountStaysOnItsLine(t *testing.T) {
	items := []map[string]any{
		{"total_price": 200.0, "price_includes_tax": true},
		{"total_price": 580.0, "tax_amount": 80.0, "tax_rate": 16.0, "price_includes_tax": true},
	}
	shareOrderDiscountVAT(items, []float64{0, 50}, 50)
	if items[0]["paid_gross"] != 200.0 {
		t.Fatalf("exempt line %v, want 200", items[0])
	}
	if items[1]["paid_gross"] != 530.0 || items[1]["tax_amount"] != 73.1 {
		t.Fatalf("B line %v, want 530 and 73.10", items[1])
	}

	sign := []treasury.SignPOSSaleItem{
		{TotalPrice: 200, PriceIncludesTax: true},
		{TotalPrice: 580, TaxAmount: 80, TaxRate: 16, PriceIncludesTax: true},
	}
	shareSignItemsDiscountVAT(sign, []float64{0, 50}, 50)
	if sign[0].PaidGross != 200 || sign[1].PaidGross != 530 || sign[1].TaxAmount != 73.1 {
		t.Fatalf("sign items %+v", sign)
	}
}
