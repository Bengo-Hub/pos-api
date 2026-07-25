-- Modify "outlet_settings" table
ALTER TABLE "outlet_settings" ADD COLUMN "enable_records_module" boolean NULL DEFAULT false, ADD COLUMN "enable_triage_module" boolean NULL DEFAULT false, ADD COLUMN "enable_examination_module" boolean NULL DEFAULT false, ADD COLUMN "enable_lab_module" boolean NULL DEFAULT false, ADD COLUMN "require_registration_fee" boolean NULL DEFAULT false, ADD COLUMN "registration_fee_catalog_item_id" uuid NULL;
-- Modify "prescriptions" table
ALTER TABLE "prescriptions" ADD COLUMN "patient_id" uuid NULL, ADD COLUMN "visit_id" uuid NULL, ADD COLUMN "external_facility_name" character varying NULL;
-- Create index "prescription_tenant_id_patient_id" to table: "prescriptions"
CREATE INDEX "prescription_tenant_id_patient_id" ON "prescriptions" ("tenant_id", "patient_id");
-- Create index "prescription_tenant_id_visit_id" to table: "prescriptions"
CREATE INDEX "prescription_tenant_id_visit_id" ON "prescriptions" ("tenant_id", "visit_id");
-- Modify "staff_members" table
ALTER TABLE "staff_members" ADD COLUMN "license_number" character varying NULL;
-- Create "examination_records" table
CREATE TABLE "examination_records" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "visit_id" uuid NOT NULL, "examined_by" uuid NOT NULL, "chief_complaint" character varying NULL, "diagnosis" character varying NULL, "clinical_notes" character varying NULL, "lab_requested" boolean NOT NULL DEFAULT false, "prescription_id" uuid NULL, "status" character varying NOT NULL DEFAULT 'in_progress', "examined_at" timestamptz NOT NULL, "completed_at" timestamptz NULL, PRIMARY KEY ("id"));
-- Create index "examinationrecord_tenant_id_status" to table: "examination_records"
CREATE INDEX "examinationrecord_tenant_id_status" ON "examination_records" ("tenant_id", "status");
-- Create index "examinationrecord_tenant_id_visit_id" to table: "examination_records"
CREATE INDEX "examinationrecord_tenant_id_visit_id" ON "examination_records" ("tenant_id", "visit_id");
-- Create "lab_orders" table
CREATE TABLE "lab_orders" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "visit_id" uuid NOT NULL, "examination_id" uuid NULL, "ordered_by" uuid NOT NULL, "status" character varying NOT NULL DEFAULT 'ordered', "notes" character varying NULL, "ordered_at" timestamptz NOT NULL, "completed_at" timestamptz NULL, PRIMARY KEY ("id"));
-- Create index "laborder_tenant_id_status" to table: "lab_orders"
CREATE INDEX "laborder_tenant_id_status" ON "lab_orders" ("tenant_id", "status");
-- Create index "laborder_tenant_id_visit_id" to table: "lab_orders"
CREATE INDEX "laborder_tenant_id_visit_id" ON "lab_orders" ("tenant_id", "visit_id");
-- Create "lab_order_lines" table
CREATE TABLE "lab_order_lines" ("id" uuid NOT NULL, "lab_order_id" uuid NOT NULL, "test_name" character varying NOT NULL, "result" character varying NULL, "unit" character varying NULL, "reference_range" character varying NULL, "flag" character varying NOT NULL DEFAULT 'pending', "notes" character varying NULL, "resulted_by" uuid NULL, "resulted_at" timestamptz NULL, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "laborderline_lab_order_id" to table: "lab_order_lines"
CREATE INDEX "laborderline_lab_order_id" ON "lab_order_lines" ("lab_order_id");
-- Create "patients" table
CREATE TABLE "patients" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "patient_number" character varying NOT NULL, "full_name" character varying NOT NULL, "dob" timestamptz NULL, "gender" character varying NULL, "phone" character varying NULL, "id_number" character varying NULL, "address" character varying NULL, "allergy_flags" jsonb NULL, "crm_contact_id" uuid NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "patient_tenant_id_full_name" to table: "patients"
CREATE INDEX "patient_tenant_id_full_name" ON "patients" ("tenant_id", "full_name");
-- Create index "patient_tenant_id_id_number" to table: "patients"
CREATE INDEX "patient_tenant_id_id_number" ON "patients" ("tenant_id", "id_number");
-- Create index "patient_tenant_id_patient_number" to table: "patients"
CREATE UNIQUE INDEX "patient_tenant_id_patient_number" ON "patients" ("tenant_id", "patient_number");
-- Create index "patient_tenant_id_phone" to table: "patients"
CREATE INDEX "patient_tenant_id_phone" ON "patients" ("tenant_id", "phone");
-- Create "patient_visits" table
CREATE TABLE "patient_visits" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "patient_id" uuid NOT NULL, "visit_number" character varying NOT NULL, "status" character varying NOT NULL DEFAULT 'registered', "registration_fee_order_id" uuid NULL, "chief_complaint" character varying NULL, "registered_by" uuid NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "patientvisit_tenant_id_outlet_id" to table: "patient_visits"
CREATE INDEX "patientvisit_tenant_id_outlet_id" ON "patient_visits" ("tenant_id", "outlet_id");
-- Create index "patientvisit_tenant_id_patient_id" to table: "patient_visits"
CREATE INDEX "patientvisit_tenant_id_patient_id" ON "patient_visits" ("tenant_id", "patient_id");
-- Create index "patientvisit_tenant_id_status" to table: "patient_visits"
CREATE INDEX "patientvisit_tenant_id_status" ON "patient_visits" ("tenant_id", "status");
-- Create index "patientvisit_tenant_id_visit_number" to table: "patient_visits"
CREATE UNIQUE INDEX "patientvisit_tenant_id_visit_number" ON "patient_visits" ("tenant_id", "visit_number");
-- Create "triage_records" table
CREATE TABLE "triage_records" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "visit_id" uuid NOT NULL, "taken_by" uuid NOT NULL, "bp_systolic" bigint NULL, "bp_diastolic" bigint NULL, "temperature_celsius" double precision NULL, "pulse_bpm" bigint NULL, "respiration_rate" bigint NULL, "spo2_percent" double precision NULL, "weight_kg" double precision NULL, "height_cm" double precision NULL, "notes" character varying NULL, "taken_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "triagerecord_tenant_id_visit_id" to table: "triage_records"
CREATE INDEX "triagerecord_tenant_id_visit_id" ON "triage_records" ("tenant_id", "visit_id");
