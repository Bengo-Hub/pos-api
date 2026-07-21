-- Create "customer_balance_caches" table
CREATE TABLE "customer_balance_caches" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "crm_contact_id" uuid NULL, "customer_identifier" character varying NULL, "customer_name" character varying NULL, "balance_due" character varying NOT NULL DEFAULT '0', "outstanding_debit" character varying NOT NULL DEFAULT '0', "store_credit_balance" character varying NOT NULL DEFAULT '0', "currency" character varying NOT NULL DEFAULT 'KES', "synced_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "customerbalancecache_tenant_id_crm_contact_id" to table: "customer_balance_caches"
CREATE UNIQUE INDEX "customerbalancecache_tenant_id_crm_contact_id" ON "customer_balance_caches" ("tenant_id", "crm_contact_id");
-- Create index "customerbalancecache_tenant_id_customer_identifier" to table: "customer_balance_caches"
CREATE INDEX "customerbalancecache_tenant_id_customer_identifier" ON "customer_balance_caches" ("tenant_id", "customer_identifier");
