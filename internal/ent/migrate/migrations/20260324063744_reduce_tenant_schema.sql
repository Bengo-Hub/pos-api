-- Modify "tenants" table
ALTER TABLE "tenants" ADD COLUMN "sync_status" character varying NOT NULL DEFAULT 'synced', ADD COLUMN "last_sync_at" timestamptz NULL;
