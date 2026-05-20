-- Modify "webhook_subscriptions" table
ALTER TABLE "webhook_subscriptions" ADD COLUMN "outlet_id" uuid NULL, ADD COLUMN "target_url" character varying NOT NULL, ADD COLUMN "secret" character varying NULL, ADD COLUMN "is_active" boolean NOT NULL DEFAULT true, ADD COLUMN "updated_at" timestamptz NOT NULL;
-- Create index "webhooksubscription_tenant_id" to table: "webhook_subscriptions"
CREATE INDEX "webhooksubscription_tenant_id" ON "webhook_subscriptions" ("tenant_id");
-- Create index "webhooksubscription_tenant_id_event_type" to table: "webhook_subscriptions"
CREATE INDEX "webhooksubscription_tenant_id_event_type" ON "webhook_subscriptions" ("tenant_id", "event_type");
-- Create "webhook_deliveries" table
CREATE TABLE "webhook_deliveries" ("id" uuid NOT NULL, "subscription_id" uuid NOT NULL, "event_type" character varying NOT NULL, "payload" text NOT NULL, "http_status" bigint NULL, "response_body" text NULL, "error_message" character varying NULL, "attempt" bigint NOT NULL DEFAULT 1, "status" character varying NOT NULL DEFAULT 'pending', "delivered_at" timestamptz NULL, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "webhookdelivery_status" to table: "webhook_deliveries"
CREATE INDEX "webhookdelivery_status" ON "webhook_deliveries" ("status");
-- Create index "webhookdelivery_subscription_id" to table: "webhook_deliveries"
CREATE INDEX "webhookdelivery_subscription_id" ON "webhook_deliveries" ("subscription_id");
