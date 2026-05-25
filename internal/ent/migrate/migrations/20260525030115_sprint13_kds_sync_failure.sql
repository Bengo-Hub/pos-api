-- Modify "kds_tickets" table
ALTER TABLE "kds_tickets" ADD COLUMN "table_reference" character varying NULL;
-- Create "kds_sync_failures" table
CREATE TABLE "kds_sync_failures" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "station_id" uuid NULL, "order_id" uuid NULL, "event_type" character varying NOT NULL, "payload" text NOT NULL, "error_message" character varying NOT NULL, "attempt" bigint NOT NULL DEFAULT 1, "status" character varying NOT NULL DEFAULT 'failed', "created_at" timestamptz NOT NULL, "resolved_at" timestamptz NULL, PRIMARY KEY ("id"));
-- Create index "kdssyncfailure_tenant_id_event_type" to table: "kds_sync_failures"
CREATE INDEX "kdssyncfailure_tenant_id_event_type" ON "kds_sync_failures" ("tenant_id", "event_type");
-- Create index "kdssyncfailure_tenant_id_status" to table: "kds_sync_failures"
CREATE INDEX "kdssyncfailure_tenant_id_status" ON "kds_sync_failures" ("tenant_id", "status");
