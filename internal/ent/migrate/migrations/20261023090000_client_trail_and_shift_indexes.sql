-- Modify "audit_logs" table: client trail (device, browser, IP) per audit entry
ALTER TABLE "audit_logs" ADD COLUMN IF NOT EXISTS "metadata" jsonb NULL;
-- Create index "pospayment_order_id" to table: "pos_payments"
CREATE INDEX IF NOT EXISTS "pospayment_order_id" ON "pos_payments" ("order_id");
-- Create index "pospayment_occurred_at" to table: "pos_payments"
CREATE INDEX IF NOT EXISTS "pospayment_occurred_at" ON "pos_payments" ("occurred_at");
-- Create index "posdevicesession_tenant_id_user_id_session_status" to table: "pos_device_sessions"
CREATE INDEX IF NOT EXISTS "posdevicesession_tenant_id_user_id_session_status" ON "pos_device_sessions" ("tenant_id", "user_id", "session_status");
-- Create index "posdevicesession_tenant_id_device_id_session_status" to table: "pos_device_sessions"
CREATE INDEX IF NOT EXISTS "posdevicesession_tenant_id_device_id_session_status" ON "pos_device_sessions" ("tenant_id", "device_id", "session_status");
-- Create index "posdevicesession_tenant_id_opened_at" to table: "pos_device_sessions"
CREATE INDEX IF NOT EXISTS "posdevicesession_tenant_id_opened_at" ON "pos_device_sessions" ("tenant_id", "opened_at");
-- Create index "posorder_tenant_id_user_id_created_at" to table: "pos_orders"
CREATE INDEX IF NOT EXISTS "posorder_tenant_id_user_id_created_at" ON "pos_orders" ("tenant_id", "user_id", "created_at");
-- Create index "posrefund_order_id" to table: "pos_refunds"
CREATE INDEX IF NOT EXISTS "posrefund_order_id" ON "pos_refunds" ("order_id");
