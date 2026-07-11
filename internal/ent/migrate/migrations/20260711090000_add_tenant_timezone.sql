-- Modify "tenants" table
ALTER TABLE "tenants" ADD COLUMN "timezone" character varying NOT NULL DEFAULT 'Africa/Nairobi';
