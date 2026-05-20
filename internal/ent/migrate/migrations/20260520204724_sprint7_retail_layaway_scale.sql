-- Modify "catalog_items" table
ALTER TABLE "catalog_items" ADD COLUMN "requires_serial" boolean NOT NULL DEFAULT false, ADD COLUMN "minimum_age" bigint NULL;
-- Modify "pos_order_lines" table
ALTER TABLE "pos_order_lines" ADD COLUMN "weight_grams" bigint NULL, ADD COLUMN "lot_number" character varying NULL, ADD COLUMN "expiry_date" timestamptz NULL;
-- Modify "staff_members" table
ALTER TABLE "staff_members" ADD COLUMN "role" character varying NOT NULL DEFAULT 'cashier';
-- Create "layaway_payments" table
CREATE TABLE "layaway_payments" ("id" uuid NOT NULL, "layaway_plan_id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "amount" double precision NOT NULL, "payment_method" character varying NOT NULL DEFAULT 'cash', "reference" character varying NULL, "notes" character varying NULL, "recorded_by" uuid NULL, "paid_at" timestamptz NOT NULL, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "layawaypayment_layaway_plan_id" to table: "layaway_payments"
CREATE INDEX "layawaypayment_layaway_plan_id" ON "layaway_payments" ("layaway_plan_id");
-- Create index "layawaypayment_tenant_id" to table: "layaway_payments"
CREATE INDEX "layawaypayment_tenant_id" ON "layaway_payments" ("tenant_id");
-- Create "layaway_plans" table
CREATE TABLE "layaway_plans" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "order_id" uuid NULL, "customer_name" character varying NOT NULL, "customer_phone" character varying NULL, "customer_email" character varying NULL, "total_amount" double precision NOT NULL, "deposit_amount" double precision NOT NULL, "paid_amount" double precision NOT NULL, "remaining_amount" double precision NOT NULL, "status" character varying NOT NULL DEFAULT 'active', "notes" character varying NULL, "due_date" timestamptz NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "layawayplan_order_id" to table: "layaway_plans"
CREATE INDEX "layawayplan_order_id" ON "layaway_plans" ("order_id");
-- Create index "layawayplan_outlet_id" to table: "layaway_plans"
CREATE INDEX "layawayplan_outlet_id" ON "layaway_plans" ("outlet_id");
-- Create index "layawayplan_status" to table: "layaway_plans"
CREATE INDEX "layawayplan_status" ON "layaway_plans" ("status");
-- Create index "layawayplan_tenant_id" to table: "layaway_plans"
CREATE INDEX "layawayplan_tenant_id" ON "layaway_plans" ("tenant_id");
-- Create "weighing_scale_readings" table
CREATE TABLE "weighing_scale_readings" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "session_id" uuid NULL, "device_serial" character varying NULL, "weight_kg" double precision NOT NULL, "unit" character varying NOT NULL DEFAULT 'kg', "catalog_item_id" uuid NULL, "status" character varying NOT NULL DEFAULT 'captured', "read_at" timestamptz NOT NULL, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "weighingscalereading_catalog_item_id" to table: "weighing_scale_readings"
CREATE INDEX "weighingscalereading_catalog_item_id" ON "weighing_scale_readings" ("catalog_item_id");
-- Create index "weighingscalereading_outlet_id" to table: "weighing_scale_readings"
CREATE INDEX "weighingscalereading_outlet_id" ON "weighing_scale_readings" ("outlet_id");
-- Create index "weighingscalereading_session_id" to table: "weighing_scale_readings"
CREATE INDEX "weighingscalereading_session_id" ON "weighing_scale_readings" ("session_id");
-- Create index "weighingscalereading_tenant_id" to table: "weighing_scale_readings"
CREATE INDEX "weighingscalereading_tenant_id" ON "weighing_scale_readings" ("tenant_id");
-- Modify "daily_closings" table
ALTER TABLE "daily_closings" DROP CONSTRAINT "daily_closings_outlet_id_fkey", ALTER COLUMN "id" DROP DEFAULT, ALTER COLUMN "drawer_ids" DROP DEFAULT, ALTER COLUMN "created_at" DROP DEFAULT, ALTER COLUMN "updated_at" DROP DEFAULT, ADD CONSTRAINT "daily_closings_outlets_daily_closings" FOREIGN KEY ("outlet_id") REFERENCES "outlets" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION;
-- Create index "dailyclosing_tenant_id_outlet_id_business_date" to table: "daily_closings"
CREATE UNIQUE INDEX "dailyclosing_tenant_id_outlet_id_business_date" ON "daily_closings" ("tenant_id", "outlet_id", "business_date");
-- Modify "pos_returns" table
ALTER TABLE "pos_returns" ALTER COLUMN "id" DROP DEFAULT, ALTER COLUMN "metadata" DROP DEFAULT, ALTER COLUMN "created_at" DROP DEFAULT, ALTER COLUMN "updated_at" DROP DEFAULT;
-- Create index "pos_returns_return_number_key" to table: "pos_returns"
CREATE UNIQUE INDEX "pos_returns_return_number_key" ON "pos_returns" ("return_number");
-- Create index "posreturn_tenant_id_order_id" to table: "pos_returns"
CREATE INDEX "posreturn_tenant_id_order_id" ON "pos_returns" ("tenant_id", "order_id");
-- Create index "posreturn_tenant_id_return_number" to table: "pos_returns"
CREATE UNIQUE INDEX "posreturn_tenant_id_return_number" ON "pos_returns" ("tenant_id", "return_number");
-- Modify "pos_return_lines" table
ALTER TABLE "pos_return_lines" DROP CONSTRAINT "pos_return_lines_return_id_fkey", ALTER COLUMN "id" DROP DEFAULT, ADD CONSTRAINT "pos_return_lines_pos_returns_lines" FOREIGN KEY ("return_id") REFERENCES "pos_returns" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION;
