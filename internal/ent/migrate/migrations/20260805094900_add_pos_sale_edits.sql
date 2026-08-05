-- Create "pos_sale_edits" table
CREATE TABLE "pos_sale_edits" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "order_id" uuid NOT NULL, "order_number" character varying NOT NULL, "kind" character varying NOT NULL, "fiscalized_at_time" boolean NOT NULL DEFAULT false, "status" character varying NOT NULL DEFAULT 'pending', "lines_before" jsonb NOT NULL, "lines_after" jsonb NOT NULL, "linked_reversal_id" uuid NULL, "linked_return_id" uuid NULL, "linked_addendum_order_id" uuid NULL, "steps" jsonb NOT NULL, "amount" double precision NOT NULL DEFAULT 0, "tax_amount" double precision NOT NULL DEFAULT 0, "cost_amount" double precision NOT NULL DEFAULT 0, "reason" character varying NOT NULL, "requested_by" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "possaleedit_tenant_id_order_id" to table: "pos_sale_edits"
CREATE INDEX "possaleedit_tenant_id_order_id" ON "pos_sale_edits" ("tenant_id", "order_id");
-- Create index "possaleedit_tenant_id_status" to table: "pos_sale_edits"
CREATE INDEX "possaleedit_tenant_id_status" ON "pos_sale_edits" ("tenant_id", "status");
