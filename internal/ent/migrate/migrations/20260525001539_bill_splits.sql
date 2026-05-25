-- Create "bill_splits" table
CREATE TABLE "bill_splits" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "order_id" uuid NOT NULL, "split_label" character varying NOT NULL, "amount" double precision NOT NULL, "currency" character varying NOT NULL DEFAULT 'KES', "status" character varying NOT NULL DEFAULT 'pending', "payment_method" character varying NULL, "external_ref" character varying NULL, "payment_id" uuid NULL, PRIMARY KEY ("id"));
-- Create index "billsplit_tenant_id_order_id" to table: "bill_splits"
CREATE INDEX "billsplit_tenant_id_order_id" ON "bill_splits" ("tenant_id", "order_id");
-- Create index "billsplit_tenant_id_status" to table: "bill_splits"
CREATE INDEX "billsplit_tenant_id_status" ON "bill_splits" ("tenant_id", "status");
