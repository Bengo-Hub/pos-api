-- Modify "pos_catalog_overrides" table
ALTER TABLE "pos_catalog_overrides" ADD COLUMN "tax_code_id" character varying NULL, ADD COLUMN "price_includes_tax" boolean NOT NULL DEFAULT false;
-- Modify "pos_order_lines" table
ALTER TABLE "pos_order_lines" ADD COLUMN "tax_code_id" character varying NULL, ADD COLUMN "tax_kra_code" character varying NULL, ADD COLUMN "tax_rate" double precision NULL, ADD COLUMN "tax_amount" double precision NULL, ADD COLUMN "price_includes_tax" boolean NOT NULL DEFAULT false;
