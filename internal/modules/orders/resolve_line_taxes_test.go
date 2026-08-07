package orders

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
)

// sqlite3 driver registration lives in settlement_test.go (same package) — reused here.

func newTestOrdersService(t *testing.T) (*Service, *ent.Client) {
	t.Helper()
	client := enttest.Open(t, "sqlite3", "file:orders_resolvetax_"+uuid.NewString()+"?mode=memory&cache=shared")
	t.Cleanup(func() { _ = client.Close() })
	return NewService(client, Config{DefaultCurrency: "KES"}, zap.NewNop()), client
}

// TestResolveLineTaxes_AppliesOutletFallback is the regression test for the live bug found
// 2026-08-07: a line with no catalog tax code and no till-provided rate got its tax applied ONLY
// at the order-total level (via calculateTotalsWithTaxes' own separate fallback branch), never
// onto the POSOrderLine row itself — so the very next totals recompute-from-lines
// (RecomputeTotalsWithClient, EditOrderLine, VoidOrderLine, AddOrderLines — all of which sum only
// line.TaxAmount) silently dropped it. ResolveLineTaxes must now resolve the SAME fallback amount
// the caller will persist onto the line, with HasInfo=true so the caller's `if lt.Amount > 0`
// gate actually fires.
func TestResolveLineTaxes_AppliesOutletFallback(t *testing.T) {
	svc, _ := newTestOrdersService(t)
	lines := []OrderLineInput{{SKU: "NO-CATALOG-TAX", UnitPrice: 100, Quantity: 1}}
	fallbackRate := decimal.NewFromFloat(0.16) // 16%

	got := svc.ResolveLineTaxes(context.Background(), uuid.New(), "test-tenant", lines, fallbackRate)
	if len(got) != 1 {
		t.Fatalf("expected 1 resolved tax, got %d", len(got))
	}
	if !got[0].HasInfo {
		t.Fatal("HasInfo = false, want true (the fallback rate must count as a definitive resolution)")
	}
	if got[0].Rate != 16 {
		t.Errorf("Rate = %v, want 16 (percent, not the 0.16 fraction)", got[0].Rate)
	}
	if got[0].Amount != 16 {
		t.Errorf("Amount = %v, want 16 (100 * 16%%)", got[0].Amount)
	}
}

// TestResolveLineTaxes_NoFallbackWhenRateZero confirms a VAT-disabled outlet (fallbackRate=0,
// what OutletFallbackTaxRate returns when OutletSetting.VatEnabled is false) never fabricates tax.
func TestResolveLineTaxes_NoFallbackWhenRateZero(t *testing.T) {
	svc, _ := newTestOrdersService(t)
	lines := []OrderLineInput{{SKU: "NO-CATALOG-TAX", UnitPrice: 100, Quantity: 1}}

	got := svc.ResolveLineTaxes(context.Background(), uuid.New(), "test-tenant", lines, decimal.Zero)
	if got[0].HasInfo || got[0].Amount != 0 {
		t.Errorf("got %+v, want a zero/no-info resolution when fallbackRate is 0", got[0])
	}
}

// TestResolveLineTaxes_FallbackSkipsInclusiveLine confirms the fallback never taxes a line whose
// price already carries its VAT embedded (PriceIncludesTax) — mirrors
// calculateTotalsWithTaxes' original !line.PriceIncludesTax gate, now enforced inside
// ResolveLineTaxes itself.
func TestResolveLineTaxes_FallbackSkipsInclusiveLine(t *testing.T) {
	svc, _ := newTestOrdersService(t)
	lines := []OrderLineInput{{SKU: "INCLUSIVE-ITEM", UnitPrice: 116, Quantity: 1, PriceIncludesTax: true}}

	got := svc.ResolveLineTaxes(context.Background(), uuid.New(), "test-tenant", lines, decimal.NewFromFloat(0.16))
	if got[0].HasInfo || got[0].Amount != 0 {
		t.Errorf("got %+v, want no fallback applied to an inclusive-price line", got[0])
	}
}

// TestResolveLineTaxes_FallbackSkipsExemptLine confirms an explicitly exempt/zero-rated line is
// never taxed by the fallback either.
func TestResolveLineTaxes_FallbackSkipsExemptLine(t *testing.T) {
	svc, _ := newTestOrdersService(t)
	lines := []OrderLineInput{{SKU: "EXEMPT-ITEM", UnitPrice: 100, Quantity: 1, TaxStatus: "zero_rated"}}

	got := svc.ResolveLineTaxes(context.Background(), uuid.New(), "test-tenant", lines, decimal.NewFromFloat(0.16))
	if got[0].Amount != 0 {
		t.Errorf("Amount = %v, want 0 (zero_rated line must never be fallback-taxed)", got[0].Amount)
	}
}

// TestRecomputeTotalsWithClient_PreservesFallbackDerivedTax is the end-to-end regression test:
// once a line's TaxAmount/TaxRate are persisted (as CreateOrder/AddOrderLines now do via
// ResolveLineTaxes' fallback tier), a later totals recompute-from-lines — the same function
// every Edit-Sale/void/add-line path calls — must NOT drop that tax. Before this fix, a
// fallback-taxed order's line never had TaxAmount persisted at all, so this exact recompute is
// what silently zeroed tax_total the instant any line changed (confirmed live: order tax_total
// 16 → 0 after one Edit-Sale edit).
func TestRecomputeTotalsWithClient_PreservesFallbackDerivedTax(t *testing.T) {
	svc, client := newTestOrdersService(t)
	_ = svc
	tenantID := uuid.New()
	ctx := context.Background()

	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(100).SetTaxTotal(16).SetTotalAmount(116).SetPaidTotal(0).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	// Mirrors what CreateOrder now persists for a fallback-VAT line: TaxAmount/TaxRate set even
	// though there's no TaxCodeID (no real catalog tax resolution — purely the outlet fallback).
	if _, err := client.POSOrderLine.Create().
		SetOrderID(order.ID).SetCatalogItemID(uuid.New()).SetSku("FALLBACK-TAXED").SetName("item").
		SetQuantity(1).SetUnitPrice(100).SetTotalPrice(100).
		SetTaxRate(16).SetTaxAmount(16).SetPriceIncludesTax(false).
		Save(ctx); err != nil {
		t.Fatalf("seed line: %v", err)
	}

	updated, err := RecomputeTotalsWithClient(ctx, client, tenantID, order.ID)
	if err != nil {
		t.Fatalf("RecomputeTotalsWithClient() error = %v", err)
	}
	if updated.TaxTotal != 16 {
		t.Errorf("TaxTotal after recompute = %v, want 16 (must survive, not vanish)", updated.TaxTotal)
	}
	if updated.TotalAmount != 116 {
		t.Errorf("TotalAmount after recompute = %v, want 116", updated.TotalAmount)
	}
}
