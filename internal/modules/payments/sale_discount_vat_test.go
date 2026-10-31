package payments

import "testing"

func TestShareOrderDiscountVAT(t *testing.T) {
	items := []map[string]any{
		{"total_price": 500.0, "tax_amount": 68.97, "tax_rate": 16.0, "price_includes_tax": true},
		{"total_price": 300.0, "price_includes_tax": true},
	}
	shareOrderDiscountVAT(items, 100)
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
	shareOrderDiscountVAT(plain, 0)
	if _, has := plain[0]["paid_gross"]; has || plain[0]["tax_amount"] != 16.0 {
		t.Fatalf("an undiscounted sale must go out as recorded: %v", plain[0])
	}
}
