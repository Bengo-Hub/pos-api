-- Add selling_price and outlet_id to catalog_items for per-outlet POS pricing.
-- selling_price overrides inventory-api pricing tiers in the proxy.
-- outlet_id (nullable) scopes the override to a specific outlet.

ALTER TABLE "catalog_items"
    ADD COLUMN IF NOT EXISTS "selling_price" double precision,
    ADD COLUMN IF NOT EXISTS "outlet_id" uuid;

-- atlas:sum file:20260521200000_add_catalog_selling_price.sql
