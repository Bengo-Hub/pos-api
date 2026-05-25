-- Replace catalog_items with pos_catalog_overrides.
-- pos-api no longer owns item data; inventory-api is the source of truth.
-- POSCatalogOverride stores only POS-specific pricing/compliance overrides per SKU.

-- Drop FK from price_book_items -> catalog_items before dropping catalog_items.
ALTER TABLE "price_book_items" DROP CONSTRAINT IF EXISTS "price_book_items_catalog_items_price_book_items";

-- Drop catalog_items (pos-api no longer owns item data).
DROP TABLE IF EXISTS "catalog_items";

-- Create "pos_catalog_overrides" table
CREATE TABLE "pos_catalog_overrides" (
  "id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "outlet_id" uuid NULL,
  "inventory_sku" character varying NOT NULL,
  "selling_price" double precision NULL,
  "currency" character varying NOT NULL DEFAULT 'KES',
  "tax_status" character varying NOT NULL DEFAULT 'taxable',
  "is_available" boolean NOT NULL DEFAULT true,
  "is_featured" boolean NOT NULL DEFAULT false,
  "display_order" bigint NOT NULL DEFAULT 0,
  "requires_prescription" boolean NOT NULL DEFAULT false,
  "is_returnable" boolean NOT NULL DEFAULT true,
  "requires_age_verification" boolean NOT NULL DEFAULT false,
  "is_controlled_substance" boolean NOT NULL DEFAULT false,
  "minimum_age" bigint NULL,
  "duration_minutes" bigint NULL,
  "metadata" jsonb NOT NULL,
  "created_at" timestamptz NOT NULL,
  "updated_at" timestamptz NOT NULL,
  PRIMARY KEY ("id")
);

-- Indexes on pos_catalog_overrides
CREATE INDEX "poscatalogoverride_tenant_id_inventory_sku_outlet_id" ON "pos_catalog_overrides" ("tenant_id", "inventory_sku", "outlet_id");
CREATE INDEX "poscatalogoverride_tenant_id_outlet_id" ON "pos_catalog_overrides" ("tenant_id", "outlet_id");
