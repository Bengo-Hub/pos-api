package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

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
}
