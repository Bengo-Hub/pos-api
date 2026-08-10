package catalog

import (
	"context"
	"database/sql"
	"os"
	"testing"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/bengobox/pos-service/internal/ent"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
)

// testPostgresDSN is the local dev Postgres instance documented in
// feedback_ent_atlas_migrations.md (PostgreSQL 17, db "pos", user/password "postgres").
// upsertSyncedCatalogOverride/upsertCatalogCostOnly issue raw Postgres-dialect SQL (now(), jsonb
// ||, a partial-unique-index ON CONFLICT target) that SQLite cannot execute — these two functions
// are genuinely Postgres-specific, so they're tested against real Postgres rather than the
// SQLite-in-memory harness used elsewhere in this codebase. Skips (not fails) when unreachable,
// e.g. in a CI runner without a local Postgres.
const testPostgresDSN = "postgres://postgres:postgres@localhost:5432/pos?sslmode=disable"

func newCatalogEventTestHandler(t *testing.T) (*InventoryEventHandler, *ent.Client) {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testPostgresDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable (expected at %s): %v", testPostgresDSN, err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	drv := entsql.OpenDB(dialect.Postgres, sqlDB)
	client := ent.NewClient(ent.Driver(drv))
	t.Cleanup(func() { _ = client.Close() })

	return NewInventoryEventHandler(client, sqlDB, nil, zap.NewNop()), client
}

// TestSyncCatalogItem_CachesCostManufacturerCategory guards the exact fix: an item.updated event
// must cache cost_price, manufacturer, and category_name into metadata (all three, alongside the
// pre-existing uom) — this is what lets resolveUnitCostsBySKU/resolveManufacturerCategoryBySKU
// answer from the local DB with zero inventory-api calls.
func TestSyncCatalogItem_CachesCostManufacturerCategory(t *testing.T) {
	h, client := newCatalogEventTestHandler(t)
	ctx := context.Background()
	tid := uuid.New()
	t.Cleanup(func() { _, _ = client.POSCatalogOverride.Delete().Where(entoverride.TenantID(tid)).Exec(context.Background()) })

	evt := sharedevents.NewEvent("item.updated", "inventory", uuid.New(), tid, map[string]any{
		"sku":           "SKU-1",
		"type":          "GOODS",
		"is_active":     true,
		"use_case":      "RETAIL",
		"cost_price":    75.0,
		"manufacturer":  "Acme",
		"category_name": "Widgets",
		"unit_name":     "PIECE",
	})

	if err := h.syncCatalogItem(ctx, evt); err != nil {
		t.Fatalf("syncCatalogItem: %v", err)
	}

	ov, err := client.POSCatalogOverride.Query().
		Where(entoverride.TenantID(tid), entoverride.InventorySku("SKU-1")).
		Only(ctx)
	if err != nil {
		t.Fatalf("query override: %v", err)
	}
	if ov.Metadata["cost_price"] != 75.0 {
		t.Errorf("metadata[cost_price] = %v, want 75.0", ov.Metadata["cost_price"])
	}
	if ov.Metadata["manufacturer"] != "Acme" {
		t.Errorf("metadata[manufacturer] = %v, want Acme", ov.Metadata["manufacturer"])
	}
	if ov.Metadata["category_name"] != "Widgets" {
		t.Errorf("metadata[category_name] = %v, want Widgets", ov.Metadata["category_name"])
	}
	if ov.Metadata["uom"] != "PIECE" {
		t.Errorf("metadata[uom] = %v, want PIECE", ov.Metadata["uom"])
	}
	if !ov.IsAvailable {
		t.Errorf("IsAvailable = false, want true (from is_active)")
	}
	if ov.ItemUseCase != "RETAIL" {
		t.Errorf("ItemUseCase = %q, want RETAIL", ov.ItemUseCase)
	}
}

// TestSyncItemCostChanged_UpdatesOnlyCost guards the exact bug the general upsert would have
// caused: item.cost_changed (goods-receipt-driven RecomputeStandardCost) carries a thin payload
// with none of item_use_case/is_available/etc. Applying it must ONLY touch metadata.cost_price —
// every other real field set by a prior item.updated must survive untouched.
func TestSyncItemCostChanged_UpdatesOnlyCost(t *testing.T) {
	h, client := newCatalogEventTestHandler(t)
	ctx := context.Background()
	tid := uuid.New()
	t.Cleanup(func() { _, _ = client.POSCatalogOverride.Delete().Where(entoverride.TenantID(tid)).Exec(context.Background()) })

	// Seed a full row via item.updated first, as would happen in production.
	seedEvt := sharedevents.NewEvent("item.updated", "inventory", uuid.New(), tid, map[string]any{
		"sku":           "SKU-1",
		"type":          "GOODS",
		"is_active":     true,
		"use_case":      "RETAIL",
		"cost_price":    75.0,
		"manufacturer":  "Acme",
		"category_name": "Widgets",
	})
	if err := h.syncCatalogItem(ctx, seedEvt); err != nil {
		t.Fatalf("seed syncCatalogItem: %v", err)
	}

	// Now a goods-receipt-driven cost recompute fires — thin payload, no use_case/is_active/etc.
	costEvt := sharedevents.NewEvent("item.cost_changed", "inventory", uuid.New(), tid, map[string]any{
		"sku":           "SKU-1",
		"previous_cost": 75.0,
		"new_cost":      90.0,
		"source":        "goods_receipt",
	})
	if err := h.syncItemCostChanged(ctx, costEvt); err != nil {
		t.Fatalf("syncItemCostChanged: %v", err)
	}

	ov, err := client.POSCatalogOverride.Query().
		Where(entoverride.TenantID(tid), entoverride.InventorySku("SKU-1")).
		Only(ctx)
	if err != nil {
		t.Fatalf("query override: %v", err)
	}
	if ov.Metadata["cost_price"] != 90.0 {
		t.Errorf("metadata[cost_price] = %v, want 90.0 (updated)", ov.Metadata["cost_price"])
	}
	// Fields the thin event never carried must be UNCHANGED, not blanked/false.
	if ov.Metadata["manufacturer"] != "Acme" {
		t.Errorf("metadata[manufacturer] = %v, want Acme (must survive a cost-only event)", ov.Metadata["manufacturer"])
	}
	if ov.Metadata["category_name"] != "Widgets" {
		t.Errorf("metadata[category_name] = %v, want Widgets (must survive)", ov.Metadata["category_name"])
	}
	if ov.ItemUseCase != "RETAIL" {
		t.Errorf("ItemUseCase = %q, want RETAIL (must survive a cost-only event)", ov.ItemUseCase)
	}
	if !ov.IsAvailable {
		t.Errorf("IsAvailable = false, want true (must survive a cost-only event, not reset to the column default)")
	}
}

// TestSyncItemCostChanged_FirstEverSightOfSKU_CreatesMinimalRow guards the other branch: if
// item.cost_changed arrives before any item.updated/created has ever been seen for this SKU (e.g.
// event redelivery ordering), it must create a minimal row rather than fail — cost_price gets
// cached now, and a fuller item event fills in the rest whenever it arrives.
func TestSyncItemCostChanged_FirstEverSightOfSKU_CreatesMinimalRow(t *testing.T) {
	h, client := newCatalogEventTestHandler(t)
	ctx := context.Background()
	tid := uuid.New()
	t.Cleanup(func() { _, _ = client.POSCatalogOverride.Delete().Where(entoverride.TenantID(tid)).Exec(context.Background()) })

	evt := sharedevents.NewEvent("item.cost_changed", "inventory", uuid.New(), tid, map[string]any{
		"sku":      "SKU-NEW",
		"new_cost": 12.0,
		"source":   "goods_receipt",
	})
	if err := h.syncItemCostChanged(ctx, evt); err != nil {
		t.Fatalf("syncItemCostChanged: %v", err)
	}

	ov, err := client.POSCatalogOverride.Query().
		Where(entoverride.TenantID(tid), entoverride.InventorySku("SKU-NEW")).
		Only(ctx)
	if err != nil {
		t.Fatalf("expected a minimal row to exist: %v", err)
	}
	if ov.Metadata["cost_price"] != 12.0 {
		t.Errorf("metadata[cost_price] = %v, want 12.0", ov.Metadata["cost_price"])
	}
}

// TestSyncItemCostChanged_MissingSKUOrCost_NoOp guards the guard clauses: an event missing sku or
// new_cost must be a silent no-op, never a panic or a spurious row.
func TestSyncItemCostChanged_MissingSKUOrCost_NoOp(t *testing.T) {
	h, client := newCatalogEventTestHandler(t)
	ctx := context.Background()
	tid := uuid.New()
	t.Cleanup(func() { _, _ = client.POSCatalogOverride.Delete().Where(entoverride.TenantID(tid)).Exec(context.Background()) })

	noSKU := sharedevents.NewEvent("item.cost_changed", "inventory", uuid.New(), tid, map[string]any{
		"new_cost": 12.0,
	})
	if err := h.syncItemCostChanged(ctx, noSKU); err != nil {
		t.Fatalf("syncItemCostChanged (no sku): %v", err)
	}
	noCost := sharedevents.NewEvent("item.cost_changed", "inventory", uuid.New(), tid, map[string]any{
		"sku": "SKU-X",
	})
	if err := h.syncItemCostChanged(ctx, noCost); err != nil {
		t.Fatalf("syncItemCostChanged (no cost): %v", err)
	}

	count, err := client.POSCatalogOverride.Query().Where(entoverride.TenantID(tid)).Count(ctx)
	if err != nil {
		t.Fatalf("count overrides: %v", err)
	}
	if count != 0 {
		t.Errorf("expected zero rows created from incomplete events, got %d", count)
	}
}
