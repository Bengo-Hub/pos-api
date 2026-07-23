-- Modify "controlled_substance_logs" table
ALTER TABLE "controlled_substance_logs" ADD COLUMN "lot_number" character varying NULL, ADD COLUMN "lot_expiry_date" timestamptz NULL;
-- Modify "prescriptions" table
ALTER TABLE "prescriptions" ALTER COLUMN "metadata" DROP DEFAULT;
