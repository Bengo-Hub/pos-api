-- Modify "pos_orders" table
ALTER TABLE "pos_orders" ADD COLUMN "voided_reason" character varying NULL, ADD COLUMN "voided_by" uuid NULL, ADD COLUMN "voided_at" timestamptz NULL;
