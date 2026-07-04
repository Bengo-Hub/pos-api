-- Create "order_void_codes" table
CREATE TABLE "order_void_codes" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NULL, "order_id" uuid NOT NULL, "action" character varying NOT NULL DEFAULT 'order.void', "code_hash" character varying NOT NULL, "approver_user_id" uuid NOT NULL, "approver_name" character varying NULL, "reason" character varying NULL, "expires_at" timestamptz NOT NULL, "used_at" timestamptz NULL, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "ordervoidcode_tenant_id_order_id_action" to table: "order_void_codes"
CREATE INDEX "ordervoidcode_tenant_id_order_id_action" ON "order_void_codes" ("tenant_id", "order_id", "action");
