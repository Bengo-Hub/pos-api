-- Create "backup_settings" table
CREATE TABLE "backup_settings" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "auto_enabled" boolean NOT NULL DEFAULT false, "schedule_hour" bigint NOT NULL DEFAULT 2, "retention_days" bigint NOT NULL DEFAULT 4, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "backup_settings_tenant_id_key" to table: "backup_settings"
CREATE UNIQUE INDEX "backup_settings_tenant_id_key" ON "backup_settings" ("tenant_id");
