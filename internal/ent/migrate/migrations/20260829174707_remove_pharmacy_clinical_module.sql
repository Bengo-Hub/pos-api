-- Decisive removal of pharmacy/clinical workflow (migrated to hospital-service, see
-- hospital-service/hospital-api/docs/migration-pos-pharmacy.md Phase D). Drops the 12
-- tables that made up pos-api's Patient/OPD/Triage/Examination/Lab/Prescription/Controlled-
-- Substance module. Written by hand rather than via the usual Atlas replay-diff generator:
-- the local ent_dev shadow schema ended up empty after replay in this environment (an
-- ent/atlas replay-mode quirk verified not to have touched the real target schema), so the
-- generator only picked up the two column-drop ALTERs below and missed the table drops
-- entirely. Table names and columns confirmed directly against the original creation
-- migrations (20260520213213_sprint8_9_pharmacy_service.sql,
-- 20260525014738_add_pharmacy_regulatory_fields.sql, 20260725054849_opd_clinical_workflow.sql,
-- 20260727230708_lab_test_catalog_diagnoses_workflow_mode.sql). CASCADE handles inter-table FK
-- ordering among the 12 (e.g. prescription_lines -> prescriptions) without needing to hand-
-- sequence it. staff_schedules (created in the same original migration file as the pharmacy
-- tables) is NOT dropped here — it is a real, still-used staff day-of-week scheduling table,
-- unrelated to pharmacy despite the shared migration file.
DROP TABLE "drug_interaction_checks" CASCADE;
DROP TABLE "prescription_lines" CASCADE;
DROP TABLE "controlled_substance_logs" CASCADE;
DROP TABLE "prescriptions" CASCADE;
DROP TABLE "lab_order_lines" CASCADE;
DROP TABLE "lab_orders" CASCADE;
DROP TABLE "examination_records" CASCADE;
DROP TABLE "triage_records" CASCADE;
DROP TABLE "patient_visits" CASCADE;
DROP TABLE "patients" CASCADE;
DROP TABLE "lab_tests" CASCADE;
DROP TABLE "diagnosis_catalogs" CASCADE;
-- Modify "outlet_settings" table
ALTER TABLE "outlet_settings" DROP COLUMN "enable_records_module", DROP COLUMN "enable_triage_module", DROP COLUMN "enable_examination_module", DROP COLUMN "enable_lab_module", DROP COLUMN "require_registration_fee", DROP COLUMN "registration_fee_catalog_item_id", DROP COLUMN "pharmacy_workflow_mode", DROP COLUMN "require_lab_prepayment";
-- Modify "pos_catalog_overrides" table
ALTER TABLE "pos_catalog_overrides" DROP COLUMN "requires_prescription";
