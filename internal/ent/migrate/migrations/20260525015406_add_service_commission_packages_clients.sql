-- Create "client_records" table
CREATE TABLE "client_records" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NULL, "full_name" character varying NOT NULL, "phone" character varying NOT NULL, "email" character varying NULL, "date_of_birth" timestamptz NULL, "gender" character varying NULL, "notes" character varying NULL, "preferences" jsonb NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "clientrecord_tenant_id_outlet_id" to table: "client_records"
CREATE INDEX "clientrecord_tenant_id_outlet_id" ON "client_records" ("tenant_id", "outlet_id");
-- Create index "clientrecord_tenant_id_phone" to table: "client_records"
CREATE UNIQUE INDEX "clientrecord_tenant_id_phone" ON "client_records" ("tenant_id", "phone");
-- Create "commission_rules" table
CREATE TABLE "commission_rules" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NULL, "staff_member_id" uuid NULL, "catalog_item_id" uuid NULL, "rule_type" character varying NOT NULL DEFAULT 'percentage', "flat_amount" double precision NULL, "percentage" double precision NULL, "tier_rules" jsonb NULL, "is_active" boolean NOT NULL DEFAULT true, "effective_from" timestamptz NOT NULL, "effective_to" timestamptz NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "commissionrule_tenant_id_catalog_item_id" to table: "commission_rules"
CREATE INDEX "commissionrule_tenant_id_catalog_item_id" ON "commission_rules" ("tenant_id", "catalog_item_id");
-- Create index "commissionrule_tenant_id_is_active" to table: "commission_rules"
CREATE INDEX "commissionrule_tenant_id_is_active" ON "commission_rules" ("tenant_id", "is_active");
-- Create index "commissionrule_tenant_id_staff_member_id" to table: "commission_rules"
CREATE INDEX "commissionrule_tenant_id_staff_member_id" ON "commission_rules" ("tenant_id", "staff_member_id");
-- Create "service_packages" table
CREATE TABLE "service_packages" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NULL, "name" character varying NOT NULL, "description" character varying NULL, "price" double precision NOT NULL, "currency" character varying NOT NULL DEFAULT 'KES', "sessions_total" bigint NOT NULL, "validity_days" bigint NOT NULL DEFAULT 365, "applicable_services" jsonb NULL, "is_active" boolean NOT NULL DEFAULT true, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "servicepackage_tenant_id_is_active" to table: "service_packages"
CREATE INDEX "servicepackage_tenant_id_is_active" ON "service_packages" ("tenant_id", "is_active");
-- Create index "servicepackage_tenant_id_outlet_id" to table: "service_packages"
CREATE INDEX "servicepackage_tenant_id_outlet_id" ON "service_packages" ("tenant_id", "outlet_id");
-- Create "service_package_purchases" table
CREATE TABLE "service_package_purchases" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "package_id" uuid NOT NULL, "client_name" character varying NOT NULL, "client_phone" character varying NOT NULL, "pos_order_id" uuid NULL, "sessions_used" bigint NOT NULL DEFAULT 0, "sessions_remaining" bigint NOT NULL, "expires_at" timestamptz NOT NULL, "status" character varying NOT NULL DEFAULT 'active', "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "servicepackagepurchase_tenant_id_client_phone" to table: "service_package_purchases"
CREATE INDEX "servicepackagepurchase_tenant_id_client_phone" ON "service_package_purchases" ("tenant_id", "client_phone");
-- Create index "servicepackagepurchase_tenant_id_package_id" to table: "service_package_purchases"
CREATE INDEX "servicepackagepurchase_tenant_id_package_id" ON "service_package_purchases" ("tenant_id", "package_id");
-- Create index "servicepackagepurchase_tenant_id_status" to table: "service_package_purchases"
CREATE INDEX "servicepackagepurchase_tenant_id_status" ON "service_package_purchases" ("tenant_id", "status");
-- Create "service_package_redemptions" table
CREATE TABLE "service_package_redemptions" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "purchase_id" uuid NOT NULL, "pos_order_id" uuid NULL, "redeemed_by" uuid NOT NULL, "service_catalog_item_id" uuid NULL, "redeemed_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "servicepackageredemption_tenant_id_purchase_id" to table: "service_package_redemptions"
CREATE INDEX "servicepackageredemption_tenant_id_purchase_id" ON "service_package_redemptions" ("tenant_id", "purchase_id");
-- Create index "servicepackageredemption_tenant_id_redeemed_at" to table: "service_package_redemptions"
CREATE INDEX "servicepackageredemption_tenant_id_redeemed_at" ON "service_package_redemptions" ("tenant_id", "redeemed_at");
