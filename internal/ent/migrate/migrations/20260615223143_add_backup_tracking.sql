-- Modify "daily_closings" table
ALTER TABLE "daily_closings" ADD COLUMN "total_pay_ins" double precision NOT NULL DEFAULT 0, ADD COLUMN "total_pay_outs" double precision NOT NULL DEFAULT 0, ADD COLUMN "total_cash_drops" double precision NOT NULL DEFAULT 0;
-- Modify "outlet_settings" table
ALTER TABLE "outlet_settings" ADD COLUMN "max_discount_percent" double precision NULL DEFAULT 100;
-- Modify "pos_order_lines" table
ALTER TABLE "pos_order_lines" ADD COLUMN "voided_qty" double precision NULL, ADD COLUMN "voided_reason" character varying NULL, ADD COLUMN "voided_by" uuid NULL, ADD COLUMN "voided_at" timestamptz NULL;
-- Modify "pos_orders" table
ALTER TABLE "pos_orders" ADD COLUMN "client_reference" character varying NULL, ADD COLUMN "offline_created_at" timestamptz NULL, ADD COLUMN "reprint_count" bigint NOT NULL DEFAULT 0;
-- Create index "posorder_tenant_id_client_reference" to table: "pos_orders"
CREATE UNIQUE INDEX "posorder_tenant_id_client_reference" ON "pos_orders" ("tenant_id", "client_reference");
-- Create "audit_logs" table
CREATE TABLE "audit_logs" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NULL, "actor_user_id" uuid NOT NULL, "actor_staff_id" uuid NULL, "approver_user_id" uuid NULL, "action" character varying NOT NULL, "entity_type" character varying NULL, "entity_id" character varying NULL, "reason" text NULL, "before_json" jsonb NULL, "after_json" jsonb NULL, "amount" double precision NULL, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "auditlog_tenant_id_actor_user_id" to table: "audit_logs"
CREATE INDEX "auditlog_tenant_id_actor_user_id" ON "audit_logs" ("tenant_id", "actor_user_id");
-- Create index "auditlog_tenant_id_created_at" to table: "audit_logs"
CREATE INDEX "auditlog_tenant_id_created_at" ON "audit_logs" ("tenant_id", "created_at");
-- Create index "auditlog_tenant_id_outlet_id_action" to table: "audit_logs"
CREATE INDEX "auditlog_tenant_id_outlet_id_action" ON "audit_logs" ("tenant_id", "outlet_id", "action");
-- Create "backups" table
CREATE TABLE "backups" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "name" character varying NOT NULL, "path" character varying NOT NULL, "size_bytes" bigint NOT NULL DEFAULT 0, "status" character varying NOT NULL DEFAULT 'completed', "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "backup_created_at" to table: "backups"
CREATE INDEX "backup_created_at" ON "backups" ("created_at");
-- Create index "backup_tenant_id_created_at" to table: "backups"
CREATE INDEX "backup_tenant_id_created_at" ON "backups" ("tenant_id", "created_at");
-- Create "idempotency_keys" table
CREATE TABLE "idempotency_keys" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "key" character varying NOT NULL, "endpoint" character varying NOT NULL DEFAULT '', "status" character varying NOT NULL DEFAULT 'in_flight', "response_code" bigint NOT NULL DEFAULT 0, "response_body" bytea NULL, "created_at" timestamptz NOT NULL, "expires_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "idempotencykey_expires_at" to table: "idempotency_keys"
CREATE INDEX "idempotencykey_expires_at" ON "idempotency_keys" ("expires_at");
-- Create index "idempotencykey_tenant_id_key" to table: "idempotency_keys"
CREATE UNIQUE INDEX "idempotencykey_tenant_id_key" ON "idempotency_keys" ("tenant_id", "key");
