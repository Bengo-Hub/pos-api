-- Modify "kds_tickets" table
ALTER TABLE "kds_tickets" ADD COLUMN "order_subtype" character varying NULL;
-- Modify "bill_splits" table
ALTER TABLE "bill_splits" ADD COLUMN "order_line_ids" jsonb NULL;
-- Create "held_items" table
CREATE TABLE "held_items" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "source_order_id" uuid NOT NULL, "source_line_id" uuid NULL, "catalog_item_id" character varying NULL, "sku" character varying NULL, "name" character varying NOT NULL, "quantity" double precision NOT NULL DEFAULT 1, "unit_price" double precision NOT NULL DEFAULT 0, "reason" character varying NULL, "status" character varying NOT NULL DEFAULT 'held', "held_by_user_id" uuid NOT NULL, "shift_session_id" uuid NULL, "claimed_order_id" uuid NULL, "resolved_by_user_id" uuid NULL, "resolved_at" timestamptz NULL, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "helditem_tenant_id_outlet_id_status" to table: "held_items"
CREATE INDEX "helditem_tenant_id_outlet_id_status" ON "held_items" ("tenant_id", "outlet_id", "status");
-- Create index "helditem_tenant_id_shift_session_id_status" to table: "held_items"
CREATE INDEX "helditem_tenant_id_shift_session_id_status" ON "held_items" ("tenant_id", "shift_session_id", "status");
