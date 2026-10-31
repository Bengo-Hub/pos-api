package orders

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// stubTreasury serves one tax code (VAT16, band B) and the tenant's VAT-active switch.
func stubTreasury(t *testing.T, vatActive bool) *TaxResolver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/s2s/test-tenant/taxes/VAT16":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "VAT16", "rate": 16, "tax_type": "vat", "kra_code": "B"})
		case "/api/v1/s2s/test-tenant/tax-profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"vat_active": vatActive})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return NewTaxResolver(treasury.NewClient(srv.URL, "test-key", 5*time.Second), nil, zap.NewNop())
}

// The outlet's fallback tax code resolves like any coded line: its rate and KRA band.
func TestResolveLineTaxes_FallbackTaxCode(t *testing.T) {
	svc, _ := newTestOrdersService(t)
	svc.SetTaxResolver(stubTreasury(t, true))
	lines := []OrderLineInput{{SKU: "NO-CATALOG-TAX", UnitPrice: 100, Quantity: 1}}

	got := svc.ResolveLineTaxes(context.Background(), uuid.New(), "test-tenant", lines,
		OutletTaxFallback{CodeID: "VAT16", Rate: decimal.NewFromFloat(0.08)})
	if got[0].CodeID != "VAT16" || got[0].KRACode != "B" || got[0].Rate != 16 || got[0].Amount != 16 {
		t.Fatalf("got %+v, want the VAT16 code at 16%% (band B), not the 8%% outlet rate", got[0])
	}
}

// A tenant treasury does not hold as VAT-registered charges no VAT through the fallback code, and
// the outlet's rate does not step in behind it.
func TestResolveLineTaxes_FallbackTaxCodeNotVATRegistered(t *testing.T) {
	svc, _ := newTestOrdersService(t)
	svc.SetTaxResolver(stubTreasury(t, false))
	lines := []OrderLineInput{{SKU: "NO-CATALOG-TAX", UnitPrice: 100, Quantity: 1}}

	got := svc.ResolveLineTaxes(context.Background(), uuid.New(), "test-tenant", lines,
		OutletTaxFallback{CodeID: "VAT16", Rate: decimal.NewFromFloat(0.16)})
	if got[0].Amount != 0 || got[0].Rate != 0 || got[0].KRACode != "D" {
		t.Fatalf("got %+v, want no VAT (band D) for a tenant not registered for VAT", got[0])
	}
}

// No settings row means no fallback: no rate is assumed.
func TestOutletFallbackTax_NoSettingsRow(t *testing.T) {
	svc, _ := newTestOrdersService(t)
	if fb := svc.OutletFallbackTax(context.Background(), uuid.New()); fb.CodeID != "" || !fb.Rate.IsZero() {
		t.Fatalf("got %+v, want no fallback for an outlet with no settings", fb)
	}
}

func TestOutletFallbackTax_ReadsCodeFromMetadata(t *testing.T) {
	svc, client := newTestOrdersService(t)
	ctx := context.Background()
	tid := uuid.New()
	if _, err := client.Tenant.Create().SetID(tid).SetName("Test").SetSlug("test-tenant").Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	outlet, err := client.Outlet.Create().SetTenantID(tid).SetTenantSlug("test-tenant").SetCode("MAIN").SetName("Main").Save(ctx)
	if err != nil {
		t.Fatalf("seed outlet: %v", err)
	}
	if _, err := client.OutletSetting.Create().SetOutletID(outlet.ID).SetVatEnabled(true).SetVatRate(16).
		SetMetadata(map[string]any{MetaKeyFallbackTaxCode: "VAT16"}).Save(ctx); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	fb := svc.OutletFallbackTax(ctx, outlet.ID)
	if fb.CodeID != "VAT16" || !fb.Rate.Equal(decimal.NewFromFloat(0.16)) {
		t.Fatalf("got %+v, want VAT16 and 0.16", fb)
	}
}
