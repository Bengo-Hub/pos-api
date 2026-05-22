-- Create "service_queue_entries" table
CREATE TABLE "service_queue_entries" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "customer_name" character varying NOT NULL, "customer_phone" character varying NULL, "service_name" character varying NULL, "staff_member_id" uuid NULL, "status" character varying NOT NULL DEFAULT 'waiting', "queue_position" bigint NOT NULL DEFAULT 0, "pos_order_id" uuid NULL, "notes" text NULL, "called_at" timestamptz NULL, "started_at" timestamptz NULL, "completed_at" timestamptz NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "servicequeueentry_tenant_id_outlet_id_created_at" to table: "service_queue_entries"
CREATE INDEX "servicequeueentry_tenant_id_outlet_id_created_at" ON "service_queue_entries" ("tenant_id", "outlet_id", "created_at");
-- Create index "servicequeueentry_tenant_id_outlet_id_status" to table: "service_queue_entries"
CREATE INDEX "servicequeueentry_tenant_id_outlet_id_status" ON "service_queue_entries" ("tenant_id", "outlet_id", "status");
