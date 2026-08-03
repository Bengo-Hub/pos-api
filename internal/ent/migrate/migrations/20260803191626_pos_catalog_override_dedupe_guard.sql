-- Prevent duplicate POSCatalogOverride rows per (tenant, sku[, outlet]). The prior plain (non-unique)
-- index on (tenant_id, inventory_sku, outlet_id) allowed a race in the inventory-event sync handler
-- (query-then-create, not atomic) to create multiple rows for the same item. Sale-time COGS cost
-- lookup then iterated all matching rows into a map with no ordering, so whichever duplicate landed
-- last in an unordered scan won -- silently resolving to a stale/empty cost roughly at random.
-- Existing duplicates were merged and removed via an ops data-cleanup pass prior to this migration.
--
-- outlet_id is nullable ("applies to all outlets"), and a plain multi-column UNIQUE constraint never
-- treats two NULLs as equal, so it would not have caught the tenant-wide (outlet_id IS NULL) case --
-- the one the inventory-event sync handler actually hits on every event. Two partial unique indexes
-- cover both cases explicitly.
CREATE UNIQUE INDEX "poscatalogoverride_tenant_sku_no_outlet" ON "pos_catalog_overrides" ("tenant_id", "inventory_sku") WHERE "outlet_id" IS NULL;
CREATE UNIQUE INDEX "poscatalogoverride_tenant_sku_outlet" ON "pos_catalog_overrides" ("tenant_id", "inventory_sku", "outlet_id") WHERE "outlet_id" IS NOT NULL;
