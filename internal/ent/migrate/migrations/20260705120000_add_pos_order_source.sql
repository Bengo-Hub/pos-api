-- Add a `source` column to pos_orders distinguishing where the sale originated:
-- "pos_terminal" (rung up on the POS terminal) vs "back_office" (entered via the
-- back-office "Add Sale" flow). Drives the All-Sales "Sources" filter and the
-- separate POS-only sales list. Existing rows backfill to 'pos_terminal' via DEFAULT.
ALTER TABLE "pos_orders" ADD COLUMN "source" character varying NOT NULL DEFAULT 'pos_terminal';
CREATE INDEX "posorder_tenant_id_source" ON "pos_orders" ("tenant_id", "source");
