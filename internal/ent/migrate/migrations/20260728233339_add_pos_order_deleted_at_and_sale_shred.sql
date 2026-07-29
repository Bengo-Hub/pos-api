-- Modify "pos_orders" table
ALTER TABLE "pos_orders" ADD COLUMN "deleted_at" timestamptz NULL;
-- Create "pos_sale_shreds" table
CREATE TABLE "pos_sale_shreds" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "order_id" uuid NOT NULL, "order_number" character varying NOT NULL, "reason" character varying NOT NULL, "status" character varying NOT NULL DEFAULT 'pending', "snapshot" jsonb NOT NULL, "steps" jsonb NOT NULL, "idempotency_key" character varying NULL, "requested_by" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "possaleshred_idempotency_key" to table: "pos_sale_shreds"
CREATE UNIQUE INDEX "possaleshred_idempotency_key" ON "pos_sale_shreds" ("idempotency_key");
-- Create index "possaleshred_tenant_id_order_id" to table: "pos_sale_shreds"
CREATE INDEX "possaleshred_tenant_id_order_id" ON "pos_sale_shreds" ("tenant_id", "order_id");
-- Create index "possaleshred_tenant_id_order_number" to table: "pos_sale_shreds"
CREATE INDEX "possaleshred_tenant_id_order_number" ON "pos_sale_shreds" ("tenant_id", "order_number");
