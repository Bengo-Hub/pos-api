package handlers

import "testing"

func TestApplyVariantOverrides(t *testing.T) {
	posPrice := 1499.2
	src := []inventoryProxyVariant{
		{SKU: "TSHIRT-RED-M", Price: 1200, IsActive: true},
		{SKU: "TSHIRT-RED-L", Price: 1300.4, IsActive: true},
		{SKU: "TSHIRT-BLUE-M", Price: 1250, IsActive: true},
	}
	got := applyVariantOverrides(src, map[string]overrideEntry{
		"TSHIRT-RED-M":  {sellingPrice: &posPrice, isAvailable: true},
		"TSHIRT-BLUE-M": {isAvailable: false},
	})
	if got[0].Price != 1500 {
		t.Fatalf("POS price wins and rounds up, got %v", got[0].Price)
	}
	if got[1].Price != 1301 {
		t.Fatalf("inventory price rounds up, got %v", got[1].Price)
	}
	if got[2].IsActive {
		t.Fatal("staff toggle off must disable the variant")
	}
	if src[0].Price != 1200 || !src[2].IsActive {
		t.Fatal("the cached source must not be mutated")
	}
}

func TestLowestActiveVariantPrice(t *testing.T) {
	vs := []inventoryProxyVariant{
		{Price: 900, IsActive: false},
		{Price: 0, IsActive: true},
		{Price: 1100, IsActive: true},
		{Price: 1000, IsActive: true},
	}
	if p := lowestActiveVariantPrice(vs); p != 1000 {
		t.Fatalf("from price = %v, want 1000 (inactive and unpriced skipped)", p)
	}
	if lowestActiveVariantPrice(nil) != 0 {
		t.Fatal("no variants means no from price")
	}
}

func TestVariantsMatchSearch(t *testing.T) {
	vs := []inventoryProxyVariant{{SKU: "TSHIRT-RED-M", Barcode: "6161234567890", Name: "T-shirt, Red / M"}}
	for _, q := range []string{"tshirt-red", "616123", "red / m"} {
		if !variantsMatchSearch(vs, q) {
			t.Fatalf("search %q should find the parent through its variant", q)
		}
	}
	if variantsMatchSearch(vs, "") || variantsMatchSearch(vs, "blue") {
		t.Fatal("empty or unrelated search must not match")
	}
}
