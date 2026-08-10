package orders

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
)

func newCostingTestClient(t *testing.T) *ent.Client {
	t.Helper()
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:costingtest_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestCatalogCostBySKU_ReadsLocalCache_NoNetworkCall guards the exact production bug (boi-enterprises,
// 2026-08-10): resolving cost from the local POSCatalogOverride cache must work from the DB alone —
// no inventory-api client, no network dependency, so a rate-limited/unreachable inventory-api can
// never blank out gross profit. This ALSO documents the deliberate behavior: a missing/absent cache
// row simply resolves to 0 cost, never an error.
func TestCatalogCostBySKU_ReadsLocalCache_NoNetworkCall(t *testing.T) {
	client := newCostingTestClient(t)
	ctx := context.Background()
	tid := uuid.New()

	if _, err := client.POSCatalogOverride.Create().
		SetTenantID(tid).
		SetInventorySku("SKU-1").
		SetMetadata(map[string]any{"cost_price": 42.5}).
		Save(ctx); err != nil {
		t.Fatalf("seed override: %v", err)
	}

	got := CatalogCostBySKU(ctx, client, tid, []string{"SKU-1", "SKU-MISSING"})
	if got["SKU-1"] != 42.5 {
		t.Errorf("CatalogCostBySKU[SKU-1] = %v, want 42.5", got["SKU-1"])
	}
	if got["SKU-MISSING"] != 0 {
		t.Errorf("CatalogCostBySKU[SKU-MISSING] = %v, want 0 (absent SKU, not an error)", got["SKU-MISSING"])
	}
}

// TestCatalogCostBySKU_TenantIsolation guards that a cost cached for one tenant never leaks into
// another tenant's cost lookup, even for the identical SKU string.
func TestCatalogCostBySKU_TenantIsolation(t *testing.T) {
	client := newCostingTestClient(t)
	ctx := context.Background()
	tidA, tidB := uuid.New(), uuid.New()

	if _, err := client.POSCatalogOverride.Create().
		SetTenantID(tidA).SetInventorySku("SHARED-SKU").
		SetMetadata(map[string]any{"cost_price": 100.0}).Save(ctx); err != nil {
		t.Fatalf("seed tenant A override: %v", err)
	}
	if _, err := client.POSCatalogOverride.Create().
		SetTenantID(tidB).SetInventorySku("SHARED-SKU").
		SetMetadata(map[string]any{"cost_price": 5.0}).Save(ctx); err != nil {
		t.Fatalf("seed tenant B override: %v", err)
	}

	gotA := CatalogCostBySKU(ctx, client, tidA, []string{"SHARED-SKU"})
	gotB := CatalogCostBySKU(ctx, client, tidB, []string{"SHARED-SKU"})
	if gotA["SHARED-SKU"] != 100.0 {
		t.Errorf("tenant A cost = %v, want 100.0", gotA["SHARED-SKU"])
	}
	if gotB["SHARED-SKU"] != 5.0 {
		t.Errorf("tenant B cost = %v, want 5.0", gotB["SHARED-SKU"])
	}
}

// TestCatalogCacheBySKU_ResolvesManufacturerAndCategoryAlongsideCost verifies the ONE-query design:
// cost_price, manufacturer, and category_name all come back from the same POSCatalogOverride row —
// no second query needed for MostProfitableItems' group_by=manufacturer|category rollup.
func TestCatalogCacheBySKU_ResolvesManufacturerAndCategoryAlongsideCost(t *testing.T) {
	client := newCostingTestClient(t)
	ctx := context.Background()
	tid := uuid.New()

	if _, err := client.POSCatalogOverride.Create().
		SetTenantID(tid).
		SetInventorySku("SKU-1").
		SetMetadata(map[string]any{
			"cost_price":    10.0,
			"manufacturer":  "Acme",
			"category_name": "Widgets",
		}).
		Save(ctx); err != nil {
		t.Fatalf("seed override: %v", err)
	}

	got := CatalogCacheBySKU(ctx, client, tid, []string{"SKU-1"})
	entry, ok := got["SKU-1"]
	if !ok {
		t.Fatalf("expected SKU-1 in cache result")
	}
	if entry.CostPrice != 10.0 {
		t.Errorf("CostPrice = %v, want 10.0", entry.CostPrice)
	}
	if entry.Manufacturer != "Acme" {
		t.Errorf("Manufacturer = %q, want %q", entry.Manufacturer, "Acme")
	}
	if entry.CategoryName != "Widgets" {
		t.Errorf("CategoryName = %q, want %q", entry.CategoryName, "Widgets")
	}
}

// TestCatalogCacheBySKU_EmptyInputs guards the degrade-gracefully contract every existing caller
// (sale-time COGS posting, returns/reversals) relies on: nil client or empty SKU list must never
// panic or error, just return an empty map.
func TestCatalogCacheBySKU_EmptyInputs(t *testing.T) {
	if got := CatalogCacheBySKU(context.Background(), nil, uuid.New(), []string{"X"}); len(got) != 0 {
		t.Errorf("nil client: got %v, want empty map", got)
	}
	client := newCostingTestClient(t)
	if got := CatalogCacheBySKU(context.Background(), client, uuid.New(), nil); len(got) != 0 {
		t.Errorf("empty skus: got %v, want empty map", got)
	}
}
