package catalog

import (
	"context"
	"testing"

	"github.com/google/uuid"

	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
)

// TestBackfillCatalogCost_CreatesRowAndMergesFields guards the one-time catalog-cost
// reconciliation tool's write path (handlers.CatalogCostBackfillHandler): on first sight of a SKU
// it must create a row with the given cost/manufacturer/category, exactly like
// upsertCatalogCostOnly's own "first-ever sight" behavior.
func TestBackfillCatalogCost_CreatesRowAndMergesFields(t *testing.T) {
	h, client := newCatalogEventTestHandler(t)
	ctx := context.Background()
	tid := uuid.New()
	t.Cleanup(func() {
		_, _ = client.POSCatalogOverride.Delete().Where(entoverride.TenantID(tid)).Exec(context.Background())
	})

	if err := BackfillCatalogCost(ctx, h.db, tid, "SKU-BF-1", BackfillCatalogCostFields{
		CostPrice:    16800,
		Manufacturer: "Samsung",
		CategoryName: "SMART PHONES",
	}); err != nil {
		t.Fatalf("BackfillCatalogCost: %v", err)
	}

	ov, err := client.POSCatalogOverride.Query().
		Where(entoverride.TenantID(tid), entoverride.InventorySku("SKU-BF-1")).
		Only(ctx)
	if err != nil {
		t.Fatalf("query override: %v", err)
	}
	if ov.Metadata["cost_price"] != 16800.0 {
		t.Errorf("metadata[cost_price] = %v, want 16800", ov.Metadata["cost_price"])
	}
	if ov.Metadata["manufacturer"] != "Samsung" {
		t.Errorf("metadata[manufacturer] = %v, want Samsung", ov.Metadata["manufacturer"])
	}
	if ov.Metadata["category_name"] != "SMART PHONES" {
		t.Errorf("metadata[category_name] = %v, want SMART PHONES", ov.Metadata["category_name"])
	}
}

// TestBackfillCatalogCost_NeverClobbersUnrelatedMetadata guards the merge semantics: re-running the
// backfill against a SKU that already carries OTHER metadata (e.g. uom/is_available-adjacent flags
// set by a prior item.updated event) must only touch cost_price/manufacturer/category_name, never
// blank out the rest — same jsonb `||` merge property upsertCatalogCostOnly already has.
func TestBackfillCatalogCost_NeverClobbersUnrelatedMetadata(t *testing.T) {
	h, client := newCatalogEventTestHandler(t)
	ctx := context.Background()
	tid := uuid.New()
	t.Cleanup(func() {
		_, _ = client.POSCatalogOverride.Delete().Where(entoverride.TenantID(tid)).Exec(context.Background())
	})

	if _, err := client.POSCatalogOverride.Create().
		SetTenantID(tid).
		SetInventorySku("SKU-BF-2").
		SetMetadata(map[string]any{"cost_price": 0.0, "uom": "PIECE"}).
		Save(ctx); err != nil {
		t.Fatalf("seed existing row: %v", err)
	}

	if err := BackfillCatalogCost(ctx, h.db, tid, "SKU-BF-2", BackfillCatalogCostFields{CostPrice: 920}); err != nil {
		t.Fatalf("BackfillCatalogCost: %v", err)
	}

	ov, err := client.POSCatalogOverride.Query().
		Where(entoverride.TenantID(tid), entoverride.InventorySku("SKU-BF-2")).
		Only(ctx)
	if err != nil {
		t.Fatalf("query override: %v", err)
	}
	if ov.Metadata["cost_price"] != 920.0 {
		t.Errorf("metadata[cost_price] = %v, want 920 (corrected)", ov.Metadata["cost_price"])
	}
	if ov.Metadata["uom"] != "PIECE" {
		t.Errorf("metadata[uom] = %v, want PIECE (must survive the cost-only backfill untouched)", ov.Metadata["uom"])
	}
}
