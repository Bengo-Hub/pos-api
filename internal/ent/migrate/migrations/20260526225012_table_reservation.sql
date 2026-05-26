-- Create "table_reservations" table
CREATE TABLE "table_reservations" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "table_id" uuid NULL, "guest_name" character varying NOT NULL, "guest_phone" character varying NULL, "guest_email" character varying NULL, "party_size" bigint NOT NULL DEFAULT 2, "scheduled_at" timestamptz NOT NULL, "duration_minutes" bigint NOT NULL DEFAULT 90, "status" character varying NOT NULL DEFAULT 'pending', "notes" character varying NULL, "special_requests" character varying NULL, "source" character varying NOT NULL DEFAULT 'staff', "cancellation_reason" character varying NULL, "confirmed_at" timestamptz NULL, "checked_in_at" timestamptz NULL, "cancelled_at" timestamptz NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "tablereservation_table_id_scheduled_at" to table: "table_reservations"
CREATE INDEX "tablereservation_table_id_scheduled_at" ON "table_reservations" ("table_id", "scheduled_at");
-- Create index "tablereservation_tenant_id_outlet_id_scheduled_at" to table: "table_reservations"
CREATE INDEX "tablereservation_tenant_id_outlet_id_scheduled_at" ON "table_reservations" ("tenant_id", "outlet_id", "scheduled_at");
-- Create index "tablereservation_tenant_id_status" to table: "table_reservations"
CREATE INDEX "tablereservation_tenant_id_status" ON "table_reservations" ("tenant_id", "status");
