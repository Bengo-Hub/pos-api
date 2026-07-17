-- Create "pos_reversals" table
CREATE TABLE "pos_reversals" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "order_id" uuid NOT NULL, "order_number" character varying NOT NULL, "reversal_number" character varying NOT NULL, "scope" character varying NOT NULL, "status" character varying NOT NULL DEFAULT 'pending', "reason" character varying NOT NULL, "refund_channel" character varying NOT NULL DEFAULT 'cash', "lines" jsonb NOT NULL, "amount" double precision NOT NULL DEFAULT 0, "tax_amount" double precision NOT NULL DEFAULT 0, "cost_amount" double precision NOT NULL DEFAULT 0, "steps" jsonb NOT NULL, "idempotency_key" character varying NULL, "requested_by" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "posreversal_idempotency_key" to table: "pos_reversals"
CREATE UNIQUE INDEX "posreversal_idempotency_key" ON "pos_reversals" ("idempotency_key");
-- Create index "posreversal_tenant_id_order_id" to table: "pos_reversals"
CREATE INDEX "posreversal_tenant_id_order_id" ON "pos_reversals" ("tenant_id", "order_id");
-- Create index "posreversal_tenant_id_reversal_number" to table: "pos_reversals"
CREATE UNIQUE INDEX "posreversal_tenant_id_reversal_number" ON "pos_reversals" ("tenant_id", "reversal_number");
-- Create index "posreversal_tenant_id_status" to table: "pos_reversals"
CREATE INDEX "posreversal_tenant_id_status" ON "pos_reversals" ("tenant_id", "status");
