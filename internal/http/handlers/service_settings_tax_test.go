package handlers

import (
	"testing"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// The fallback tax code the till picked comes back on GET /pos/settings from outlet metadata.
func TestSettingsResponseFallbackTaxCode(t *testing.T) {
	outlet := &ent.Outlet{}
	got := toSettingsResponse(outlet, &ent.OutletSetting{VatEnabled: true, VatRate: 16,
		Metadata: map[string]any{orders.MetaKeyFallbackTaxCode: "VAT16"}})
	if got.FallbackTaxCode != "VAT16" {
		t.Fatalf("fallback_tax_code = %q, want VAT16", got.FallbackTaxCode)
	}
	if none := toSettingsResponse(outlet, &ent.OutletSetting{}); none.FallbackTaxCode != "" {
		t.Fatalf("fallback_tax_code = %q, want empty when none was picked", none.FallbackTaxCode)
	}
}
