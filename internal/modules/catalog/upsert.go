package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// upsertSyncedItemFields is the subset of POSCatalogOverride columns syncCatalogItem projects from an
// inventory item.updated/created event.
type upsertSyncedItemFields struct {
	ItemUseCase             string
	TaxCodeID               string
	DurationMinutes         *int
	RequiresAgeVerification bool
	IsControlledSubstance   bool
	IsAvailable             bool
	InventoryItemID         *uuid.UUID
	// Metadata is merged (key-by-key) into the row's existing metadata via Postgres's jsonb `||`
	// operator, never replaced wholesale -- so a bare cost_price/uom sync can never clobber the
	// "complimentary" flag a tenant-admin price edit stored under the same column, and vice versa.
	Metadata map[string]any
}

// upsertSyncedCatalogOverride atomically creates-or-updates the TENANT-WIDE (outlet_id IS NULL)
// POSCatalogOverride row for (tenantID, sku) via a database-level ON CONFLICT upsert, targeting the
// partial unique index added in 20260803191626_pos_catalog_override_dedupe_guard.sql.
//
// This replaces a query-then-create/update pattern that raced under concurrent event delivery:
// two deliveries for the same SKU could both see "no existing row" and both INSERT, leaving a
// duplicate where the sale-time COGS cost lookup (which iterates all matching rows into a map with
// no defined order) would resolve to whichever row landed last in an unordered scan -- silently
// posting zero cost roughly at random. See the migration file for the full incident writeup.
func upsertSyncedCatalogOverride(ctx context.Context, db *sql.DB, tenantID uuid.UUID, sku string, f upsertSyncedItemFields) error {
	if db == nil {
		return fmt.Errorf("catalog: upsert called with nil db handle")
	}
	metaJSON, err := json.Marshal(f.Metadata)
	if err != nil {
		return fmt.Errorf("catalog: marshal metadata for %s: %w", sku, err)
	}
	var durationMinutes any
	if f.DurationMinutes != nil {
		durationMinutes = *f.DurationMinutes
	}
	var inventoryItemID any
	if f.InventoryItemID != nil {
		inventoryItemID = *f.InventoryItemID
	}

	const q = `
INSERT INTO pos_catalog_overrides
  (id, tenant_id, inventory_sku, outlet_id, inventory_item_id, item_use_case, tax_code_id,
   duration_minutes, requires_age_verification, is_controlled_substance, is_available, metadata,
   created_at, updated_at)
VALUES
  ($1, $2, $3, NULL, $4, $5, NULLIF($6, ''), $7, $8, $9, $10, $11::jsonb, now(), now())
ON CONFLICT (tenant_id, inventory_sku) WHERE outlet_id IS NULL
DO UPDATE SET
  inventory_item_id         = COALESCE(EXCLUDED.inventory_item_id, pos_catalog_overrides.inventory_item_id),
  item_use_case              = EXCLUDED.item_use_case,
  tax_code_id                = COALESCE(EXCLUDED.tax_code_id, pos_catalog_overrides.tax_code_id),
  duration_minutes           = COALESCE(EXCLUDED.duration_minutes, pos_catalog_overrides.duration_minutes),
  requires_age_verification  = EXCLUDED.requires_age_verification,
  is_controlled_substance    = EXCLUDED.is_controlled_substance,
  is_available                = EXCLUDED.is_available,
  metadata                    = pos_catalog_overrides.metadata || EXCLUDED.metadata,
  updated_at                  = now()`

	_, err = db.ExecContext(ctx, q,
		uuid.New(), tenantID, sku, inventoryItemID, f.ItemUseCase, f.TaxCodeID,
		durationMinutes, f.RequiresAgeVerification, f.IsControlledSubstance, f.IsAvailable, string(metaJSON),
	)
	if err != nil {
		return fmt.Errorf("catalog: upsert override for %s: %w", sku, err)
	}
	return nil
}
