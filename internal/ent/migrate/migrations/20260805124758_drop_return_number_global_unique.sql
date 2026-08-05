-- Drop index "pos_returns_return_number_key" from table: "pos_returns"
-- This is a stray GLOBAL single-column unique index that ent's field-level .Unique()
-- modifier on return_number emitted in addition to the intended composite
-- "posreturn_tenant_id_return_number" unique index (kept, unaffected). return_number only
-- needs to be unique PER TENANT — the same class of bug already fixed once for
-- pos_orders.order_number on 2026-07-26 (see 20260726101011_drop_order_number_global_unique.sql).
-- Root cause of a live 2026-08-05 failure: codevertex-demo's first-ever Edit-Sale fiscalized
-- reduction could not create its POSReturn because boi-enterprises already held return_number
-- "000001".
DROP INDEX "pos_returns_return_number_key";
