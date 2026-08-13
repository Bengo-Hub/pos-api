package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

// seedCompletedSaleWithTax is seedCompletedSale's sibling for tests that need a real VAT
// component: subtotal is the pre-tax amount, taxTotal is added on top to form total_amount
// (mirrors orders.Service.CalculateTotals' tax-exclusive formula: total = subtotal + tax).
func seedCompletedSaleWithTax(t *testing.T, client *ent.Client, tid, outletID uuid.UUID, sku, name string, subtotal, taxTotal float64) {
	t.Helper()
	total := subtotal + taxTotal
	o, err := client.POSOrder.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetDeviceID(uuid.New()).
		SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).
		SetStatus("completed").
		SetSubtotal(subtotal).
		SetTaxTotal(taxTotal).
		SetTotalAmount(total).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if _, err := client.POSOrderLine.Create().
		SetOrderID(o.ID).
		SetCatalogItemID(uuid.New()).
		SetSku(sku).
		SetName(name).
		SetQuantity(1).
		SetUnitPrice(total).
		SetTotalPrice(total).
		Save(context.Background()); err != nil {
		t.Fatalf("seed order line: %v", err)
	}
}

// TestGetSummary_GrossProfitReadsLocalCache_NotEqualToRevenue reproduces and guards the exact
// production bug (boi-enterprises, 2026-08-10): GetSummary's gross-profit card silently equalled
// revenue whenever the live inventory-api catalog walk resolveUnitCostsBySKU used to make failed
// (e.g. rate-limited on a large catalog). Now that cost is read from the local POSCatalogOverride
// cache, this test proves the whole path works with ZERO inventory-api reachability at all — this
// test process makes no network calls and there is no mock inventory-api server, yet gross_profit
// still comes back correctly below revenue.
func TestGetSummary_GrossProfitReadsLocalCache_NotEqualToRevenue(t *testing.T) {
	h, client := newReportsTestHandler(t)
	tid := uuid.New()
	outletID := uuid.New()

	// Revenue 1000, real cost 400 -> gross profit must be 600, margin 60%. Cache the cost the SAME
	// way the inventory.item.updated event consumer does (metadata["cost_price"]).
	seedCompletedSale(t, client, tid, outletID, "SKU-1", "Widget", 1000)
	if _, err := client.POSCatalogOverride.Create().
		SetTenantID(tid).
		SetInventorySku("SKU-1").
		SetMetadata(map[string]any{"cost_price": 400.0}).
		Save(context.Background()); err != nil {
		t.Fatalf("seed cost cache: %v", err)
	}

	req := reportsRequest(t, tid, &outletID, "")
	rec := httptest.NewRecorder()
	h.GetSummary(rec, req)

	if rec.Code != 200 {
		t.Fatalf("GetSummary: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	grossProfit, _ := body["gross_profit"].(float64)
	totalRevenue, _ := body["total_revenue"].(float64)

	if totalRevenue != 1000 {
		t.Fatalf("total_revenue = %v, want 1000", totalRevenue)
	}
	// THE regression this guards: without reading the cache, cost resolves to 0 and gross_profit
	// silently equals total_revenue (the exact "100% margin" bug reported live).
	if grossProfit == totalRevenue {
		t.Fatalf("gross_profit (%v) == total_revenue (%v) — cost was NOT applied (the exact reported bug: 100%% margin on a real sale)", grossProfit, totalRevenue)
	}
	if grossProfit != 600 {
		t.Errorf("gross_profit = %v, want 600 (revenue 1000 - real cached cost 400)", grossProfit)
	}
}

// TestGetSummary_GrossProfitZeroCost_WhenNoCatalogCacheRow documents the honest degrade: with NO
// cache row at all for a sold SKU (never synced yet), cost resolves to 0 and gross_profit equals
// revenue — this is the expected fallback for a genuinely unpriced item, not a bug. Distinguishes
// "no data yet" from the reported bug ("had cost data, but the live fetch that should have found
// it failed").
func TestGetSummary_GrossProfitZeroCost_WhenNoCatalogCacheRow(t *testing.T) {
	h, client := newReportsTestHandler(t)
	tid := uuid.New()
	outletID := uuid.New()

	seedCompletedSale(t, client, tid, outletID, "SKU-UNPRICED", "Mystery Item", 1000)
	// Deliberately no POSCatalogOverride row for SKU-UNPRICED.

	req := reportsRequest(t, tid, &outletID, "")
	rec := httptest.NewRecorder()
	h.GetSummary(rec, req)

	if rec.Code != 200 {
		t.Fatalf("GetSummary: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	grossProfit, _ := body["gross_profit"].(float64)
	if grossProfit != 1000 {
		t.Errorf("gross_profit = %v, want 1000 (no cost data at all is a fair 0-cost fallback)", grossProfit)
	}
	// Additive visibility (does not change the math above): a sold SKU with no cache entry at all
	// must surface via skus_missing_cost, so a coverage gap like the one that inflated
	// boi-enterprises' margin is visible instead of silently hiding inside the number.
	skusMissingCost, _ := body["skus_missing_cost"].(float64)
	if skusMissingCost != 1 {
		t.Errorf("skus_missing_cost = %v, want 1", skusMissingCost)
	}
}

// TestGetSummary_GrossProfitNetsOutVAT guards against the second confirmed bug: gross_profit must
// be computed from revenue NET of VAT (matching what treasury actually books as Revenue at
// sale-time — see finance-service/treasury-api pos/subscriber.go handleSaleFinalized, which splits
// tax_total out of the posted revenue), not from order.TotalAmount directly. A tenant charging
// VAT would otherwise see gross profit/margin overstated by the tax portion of every sale.
func TestGetSummary_GrossProfitNetsOutVAT(t *testing.T) {
	h, client := newReportsTestHandler(t)
	tid := uuid.New()
	outletID := uuid.New()

	// Subtotal 1000 + 16% VAT (160) -> total_amount 1160 (what the customer actually paid).
	// Real cost 400 -> true gross profit must be 1000-400=600 (margin 60%), NOT 1160-400=760
	// (margin ~65.5%, the pre-fix bug that counted collected VAT as if it were sales revenue).
	seedCompletedSaleWithTax(t, client, tid, outletID, "SKU-VAT", "Taxed Widget", 1000, 160)
	if _, err := client.POSCatalogOverride.Create().
		SetTenantID(tid).
		SetInventorySku("SKU-VAT").
		SetMetadata(map[string]any{"cost_price": 400.0}).
		Save(context.Background()); err != nil {
		t.Fatalf("seed cost cache: %v", err)
	}

	req := reportsRequest(t, tid, &outletID, "")
	rec := httptest.NewRecorder()
	h.GetSummary(rec, req)

	if rec.Code != 200 {
		t.Fatalf("GetSummary: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	totalRevenue, _ := body["total_revenue"].(float64)
	grossProfit, _ := body["gross_profit"].(float64)
	marginPct, _ := body["gross_margin_pct"].(float64)

	// total_revenue (the headline KPI, matches what the receipt shows) stays VAT-inclusive.
	if totalRevenue != 1160 {
		t.Fatalf("total_revenue = %v, want 1160 (VAT-inclusive, unchanged)", totalRevenue)
	}
	if grossProfit != 600 {
		t.Errorf("gross_profit = %v, want 600 (1000 net-of-VAT revenue - 400 cost)", grossProfit)
	}
	if marginPct < 59.9 || marginPct > 60.1 {
		t.Errorf("gross_margin_pct = %v, want ~60 (600/1000 net-of-VAT, not 600/1160)", marginPct)
	}
}
