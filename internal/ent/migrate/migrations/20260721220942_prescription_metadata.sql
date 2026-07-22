-- Modify "prescriptions" table
ALTER TABLE "prescriptions" ADD COLUMN "metadata" jsonb NOT NULL DEFAULT '{}';
