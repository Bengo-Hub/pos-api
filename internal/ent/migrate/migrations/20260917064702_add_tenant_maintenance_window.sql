-- Modify "tenants" table
ALTER TABLE "tenants" ADD COLUMN "maintenance_starts_at" timestamptz NULL, ADD COLUMN "maintenance_ends_at" timestamptz NULL, ADD COLUMN "maintenance_reason" character varying NULL, ADD COLUMN "maintenance_activated_by" character varying NULL;
