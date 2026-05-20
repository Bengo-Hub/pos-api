-- Create "drug_interaction_checks" table
CREATE TABLE "drug_interaction_checks" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "prescription_id" uuid NULL, "order_id" uuid NULL, "drug_skus" jsonb NOT NULL, "result" character varying NOT NULL DEFAULT 'clear', "details" text NULL, "checked_by" uuid NULL, "checked_at" timestamptz NOT NULL, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "druginteractioncheck_order_id" to table: "drug_interaction_checks"
CREATE INDEX "druginteractioncheck_order_id" ON "drug_interaction_checks" ("order_id");
-- Create index "druginteractioncheck_prescription_id" to table: "drug_interaction_checks"
CREATE INDEX "druginteractioncheck_prescription_id" ON "drug_interaction_checks" ("prescription_id");
-- Create index "druginteractioncheck_tenant_id" to table: "drug_interaction_checks"
CREATE INDEX "druginteractioncheck_tenant_id" ON "drug_interaction_checks" ("tenant_id");
-- Create "prescriptions" table
CREATE TABLE "prescriptions" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "order_id" uuid NULL, "prescription_number" character varying NOT NULL, "prescriber_name" character varying NULL, "prescriber_license" character varying NULL, "patient_name" character varying NOT NULL, "patient_dob" character varying NULL, "patient_id_number" character varying NULL, "status" character varying NOT NULL DEFAULT 'pending', "notes" character varying NULL, "dispensed_at" timestamptz NULL, "dispensed_by" uuid NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "prescription_order_id" to table: "prescriptions"
CREATE INDEX "prescription_order_id" ON "prescriptions" ("order_id");
-- Create index "prescription_outlet_id" to table: "prescriptions"
CREATE INDEX "prescription_outlet_id" ON "prescriptions" ("outlet_id");
-- Create index "prescription_prescription_number" to table: "prescriptions"
CREATE INDEX "prescription_prescription_number" ON "prescriptions" ("prescription_number");
-- Create index "prescription_status" to table: "prescriptions"
CREATE INDEX "prescription_status" ON "prescriptions" ("status");
-- Create index "prescription_tenant_id" to table: "prescriptions"
CREATE INDEX "prescription_tenant_id" ON "prescriptions" ("tenant_id");
-- Create "prescription_lines" table
CREATE TABLE "prescription_lines" ("id" uuid NOT NULL, "prescription_id" uuid NOT NULL, "catalog_item_id" uuid NULL, "drug_name" character varying NOT NULL, "dosage" character varying NULL, "form" character varying NULL, "instructions" character varying NULL, "quantity_prescribed" bigint NOT NULL, "quantity_dispensed" bigint NOT NULL DEFAULT 0, "unit_price" double precision NULL, "lot_number" character varying NULL, "expiry_date" timestamptz NULL, "status" character varying NOT NULL DEFAULT 'pending', PRIMARY KEY ("id"));
-- Create index "prescriptionline_catalog_item_id" to table: "prescription_lines"
CREATE INDEX "prescriptionline_catalog_item_id" ON "prescription_lines" ("catalog_item_id");
-- Create index "prescriptionline_prescription_id" to table: "prescription_lines"
CREATE INDEX "prescriptionline_prescription_id" ON "prescription_lines" ("prescription_id");
-- Create "staff_schedules" table
CREATE TABLE "staff_schedules" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "staff_member_id" uuid NOT NULL, "day_of_week" bigint NOT NULL, "start_time" character varying NOT NULL, "end_time" character varying NOT NULL, "is_available" boolean NOT NULL DEFAULT true, "notes" character varying NULL, PRIMARY KEY ("id"));
-- Create index "staffschedule_outlet_id" to table: "staff_schedules"
CREATE INDEX "staffschedule_outlet_id" ON "staff_schedules" ("outlet_id");
-- Create index "staffschedule_staff_member_id" to table: "staff_schedules"
CREATE INDEX "staffschedule_staff_member_id" ON "staff_schedules" ("staff_member_id");
-- Create index "staffschedule_tenant_id_staff_member_id_day_of_week" to table: "staff_schedules"
CREATE UNIQUE INDEX "staffschedule_tenant_id_staff_member_id_day_of_week" ON "staff_schedules" ("tenant_id", "staff_member_id", "day_of_week");
