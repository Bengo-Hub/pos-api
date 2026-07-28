-- Modify "examination_records" table
ALTER TABLE "examination_records" ADD COLUMN "diagnosis_codes" jsonb NULL;
-- Modify "lab_order_lines" table
ALTER TABLE "lab_order_lines" ADD COLUMN "lab_test_id" uuid NULL, ADD COLUMN "price" double precision NULL;
-- Modify "lab_orders" table
ALTER TABLE "lab_orders" ADD COLUMN "payment_order_id" uuid NULL, ADD COLUMN "total_amount" double precision NULL;
-- Modify "outlet_settings" table
ALTER TABLE "outlet_settings" ADD COLUMN "pharmacy_workflow_mode" character varying NULL DEFAULT 'direct', ADD COLUMN "require_lab_prepayment" boolean NULL DEFAULT true;
-- Create "diagnosis_catalogs" table
CREATE TABLE "diagnosis_catalogs" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "is_global" boolean NOT NULL DEFAULT false, "name" character varying NOT NULL, "code" character varying NULL, "category" character varying NULL, "is_active" boolean NOT NULL DEFAULT true, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "diagnosiscatalog_is_global" to table: "diagnosis_catalogs"
CREATE INDEX "diagnosiscatalog_is_global" ON "diagnosis_catalogs" ("is_global");
-- Create index "diagnosiscatalog_tenant_id_category" to table: "diagnosis_catalogs"
CREATE INDEX "diagnosiscatalog_tenant_id_category" ON "diagnosis_catalogs" ("tenant_id", "category");
-- Create index "diagnosiscatalog_tenant_id_name" to table: "diagnosis_catalogs"
CREATE UNIQUE INDEX "diagnosiscatalog_tenant_id_name" ON "diagnosis_catalogs" ("tenant_id", "name");
-- Create "lab_tests" table
CREATE TABLE "lab_tests" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "name" character varying NOT NULL, "code" character varying NULL, "category" character varying NULL, "price" double precision NOT NULL, "sample_type" character varying NULL, "reference_range" character varying NULL, "unit" character varying NULL, "turnaround_hours" bigint NULL, "is_active" boolean NOT NULL DEFAULT true, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "labtest_tenant_id_category" to table: "lab_tests"
CREATE INDEX "labtest_tenant_id_category" ON "lab_tests" ("tenant_id", "category");
-- Create index "labtest_tenant_id_is_active" to table: "lab_tests"
CREATE INDEX "labtest_tenant_id_is_active" ON "lab_tests" ("tenant_id", "is_active");
-- Create index "labtest_tenant_id_name" to table: "lab_tests"
CREATE UNIQUE INDEX "labtest_tenant_id_name" ON "lab_tests" ("tenant_id", "name");
