-- Modify "outlet_settings" table
ALTER TABLE "outlet_settings" ALTER COLUMN "shift_auto_end_enabled" DROP NOT NULL, ALTER COLUMN "shift_max_hours" TYPE bigint, ALTER COLUMN "shift_max_hours" DROP NOT NULL;
-- Create "pos_notifications" table
CREATE TABLE "pos_notifications" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "user_id" uuid NOT NULL, "notification_type" character varying NOT NULL, "title" character varying NOT NULL, "body" character varying NOT NULL DEFAULT '', "payload" jsonb NOT NULL, "is_read" boolean NOT NULL DEFAULT false, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "posnotification_tenant_id_outlet_id" to table: "pos_notifications"
CREATE INDEX "posnotification_tenant_id_outlet_id" ON "pos_notifications" ("tenant_id", "outlet_id");
-- Create index "posnotification_tenant_id_user_id_is_read" to table: "pos_notifications"
CREATE INDEX "posnotification_tenant_id_user_id_is_read" ON "pos_notifications" ("tenant_id", "user_id", "is_read");
