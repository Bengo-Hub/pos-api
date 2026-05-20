-- Create "loyalty_accounts" table
CREATE TABLE "loyalty_accounts" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "customer_id" uuid NULL, "customer_phone" character varying NOT NULL, "customer_name" character varying NOT NULL, "points_balance" bigint NOT NULL DEFAULT 0, "lifetime_points" bigint NOT NULL DEFAULT 0, "program_id" uuid NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "loyaltyaccount_tenant_id_customer_id" to table: "loyalty_accounts"
CREATE INDEX "loyaltyaccount_tenant_id_customer_id" ON "loyalty_accounts" ("tenant_id", "customer_id");
-- Create index "loyaltyaccount_tenant_id_customer_phone" to table: "loyalty_accounts"
CREATE UNIQUE INDEX "loyaltyaccount_tenant_id_customer_phone" ON "loyalty_accounts" ("tenant_id", "customer_phone");
-- Create "loyalty_programs" table
CREATE TABLE "loyalty_programs" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "name" character varying NOT NULL, "description" character varying NULL, "earn_rate" double precision NOT NULL DEFAULT 1, "redeem_rate" double precision NOT NULL DEFAULT 0.01, "min_redeem_points" bigint NOT NULL DEFAULT 100, "is_active" boolean NOT NULL DEFAULT true, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "loyaltyprogram_tenant_id" to table: "loyalty_programs"
CREATE INDEX "loyaltyprogram_tenant_id" ON "loyalty_programs" ("tenant_id");
-- Create "loyalty_transactions" table
CREATE TABLE "loyalty_transactions" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "account_id" uuid NOT NULL, "order_id" uuid NULL, "type_field" character varying NOT NULL, "points" bigint NOT NULL, "balance_after" bigint NOT NULL, "notes" character varying NULL, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "loyaltytransaction_account_id" to table: "loyalty_transactions"
CREATE INDEX "loyaltytransaction_account_id" ON "loyalty_transactions" ("account_id");
-- Create index "loyaltytransaction_tenant_id_order_id" to table: "loyalty_transactions"
CREATE INDEX "loyaltytransaction_tenant_id_order_id" ON "loyalty_transactions" ("tenant_id", "order_id");
