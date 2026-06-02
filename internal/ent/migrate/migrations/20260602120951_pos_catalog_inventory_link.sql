-- Modify "facilities" table
ALTER TABLE "facilities" ADD COLUMN "inventory_item_id" uuid NULL;
-- Modify "pos_catalog_overrides" table
ALTER TABLE "pos_catalog_overrides" ADD COLUMN "inventory_item_id" uuid NULL, ADD COLUMN "item_use_case" character varying NULL, ADD COLUMN "is_bundle" boolean NOT NULL DEFAULT false;
-- Create index "poscatalogoverride_tenant_id_inventory_item_id" to table: "pos_catalog_overrides"
CREATE INDEX "poscatalogoverride_tenant_id_inventory_item_id" ON "pos_catalog_overrides" ("tenant_id", "inventory_item_id");
-- Modify "room_amenities" table
ALTER TABLE "room_amenities" ADD COLUMN "inventory_item_id" uuid NULL;
-- Modify "room_folio_items" table
ALTER TABLE "room_folio_items" ADD COLUMN "inventory_sku" character varying NULL, ADD COLUMN "inventory_bundle_id" uuid NULL;
-- Modify "rooms" table
ALTER TABLE "rooms" ADD COLUMN "inventory_item_id" uuid NULL;
