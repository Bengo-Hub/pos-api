-- Drop index "pos_orders_order_number_key" from table: "pos_orders"
-- This is a stray GLOBAL single-column unique index that ent's field-level .Unique()
-- modifier on order_number emitted in addition to the intended composite
-- "posorder_tenant_id_order_number" unique index (kept, unaffected). order_number only
-- needs to be unique PER TENANT — the global constraint made every tenant's first order
-- collide with any other tenant's identically-numbered first order once order numbering
-- switched to the tenant document sequence's pure-numeric default (e.g. two tenants both
-- minting "000001"). Root cause of the 2026-07-26 "duplicate key value violates unique
-- constraint pos_orders_order_number_key" POS checkout outage.
DROP INDEX "pos_orders_order_number_key";
