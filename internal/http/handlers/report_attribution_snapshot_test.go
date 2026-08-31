package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

// seedCompletedSaleWithLineCostSnapshot mirrors seedCompletedSale but lets the caller set the
// line's quantity/unit price independently and stamp metadata["unit_cost_at_sale"] the way
// payments.Service.publishSaleFinalized does at real sale-finalize time.
func seedCompletedSaleWithLineCostSnapshot(t *testing.T, client *ent.Client, tid, outletID uuid.UUID, sku, name string, qty, unitPrice float64, unitCostAtSale *float64) {
	t.Helper()
	total := qty * unitPrice
	o, err := client.POSOrder.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetDeviceID(uuid.New()).
		SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).
		SetStatus("completed").
		SetSubtotal(total).
		SetTaxTotal(0).
		SetTotalAmount(total).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	create := client.POSOrderLine.Create().
		SetOrderID(o.ID).
		SetCatalogItemID(uuid.New()).
		SetSku(sku).
		SetName(name).
		SetQuantity(qty).
		SetUnitPrice(unitPrice).
		SetTotalPrice(total)
	if unitCostAtSale != nil {
		create = create.SetMetadata(map[string]any{"unit_cost_at_sale": *unitCostAtSale})
	}
	if _, err := create.Save(context.Background()); err != nil {
		t.Fatalf("seed order line: %v", err)
	}
}

// TestGetSummary_GrossProfit_PrefersLineSnapshotCost_OverStaleLiveCache reproduces and guards the
// small-steps-cosmetics 2026-09-01 phantom-loss bug: an item's cost_price was later changed (a
// pricing fix, or a ml->bottle unit-basis migration) AFTER a line was already sold. Before this
// fix, resolveUnitCostsBySKU always re-derived cost from TODAY's catalog cache, so a historical
// sale made under the OLD cost economics got re-priced with the NEW cost — turning a real profit
// into a fabricated multi-thousand-shilling "loss". The line's own UnitCostAtSale snapshot (see
// report_attribution.go) must win over the live cache.
func TestGetSummary_GrossProfit_PrefersLineSnapshotCost_OverStaleLiveCache(t *testing.T) {
	h, client := newReportsTestHandler(t)
	tid := uuid.New()
	outletID := uuid.New()

	// 12 units sold at 30 each (360 total) back when the item's real per-unit cost was 9 — the
	// exact ml-tracked economics small-steps-cosmetics' perfume-oil SKUs had before their cost was
	// later bumped to a per-bottle basis.
	oldCost := 9.0
	seedCompletedSaleWithLineCostSnapshot(t, client, tid, outletID, "SKU-MIGRATED", "Perfume Oil", 12, 30, &oldCost)

	// The live catalog cache now reflects the NEW, current cost (450) — set well after the sale
	// above happened, exactly like inventory-api's Item.cost_price after a unit-basis migration.
	if _, err := client.POSCatalogOverride.Create().
		SetTenantID(tid).
		SetInventorySku("SKU-MIGRATED").
		SetMetadata(map[string]any{"cost_price": 450.0}).
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

	// Correct (snapshot-based): 360 revenue - (9 x 12 = 108) cost = 252 profit.
	// The pre-fix bug would have computed 360 - (450 x 12 = 5400) = -5040, a fabricated loss.
	if grossProfit != 252 {
		t.Errorf("gross_profit = %v, want 252 (12 units x (30-9) — must use the line's sale-time cost snapshot, not today's live cache of 450)", grossProfit)
	}
}

// TestGetSummary_GrossProfit_FallsBackToLiveCache_WhenNoSnapshot guards the other half of the same
// change: a line with NO cost snapshot (sold before this feature existed, or whose cost was
// genuinely unknown at sale time) must behave exactly as before — resolved from the live
// POSCatalogOverride cache, unchanged.
func TestGetSummary_GrossProfit_FallsBackToLiveCache_WhenNoSnapshot(t *testing.T) {
	h, client := newReportsTestHandler(t)
	tid := uuid.New()
	outletID := uuid.New()

	seedCompletedSaleWithLineCostSnapshot(t, client, tid, outletID, "SKU-LEGACY", "Old Sale", 1, 1000, nil)
	if _, err := client.POSCatalogOverride.Create().
		SetTenantID(tid).
		SetInventorySku("SKU-LEGACY").
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
	if grossProfit != 600 {
		t.Errorf("gross_profit = %v, want 600 (1000 revenue - 400 live-cache cost, unchanged fallback behavior)", grossProfit)
	}
}
