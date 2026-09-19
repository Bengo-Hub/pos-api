-- Create "lost_found_items" table
CREATE TABLE "lost_found_items" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "room_id" uuid NULL, "room_guest_id" uuid NULL, "description" character varying NOT NULL, "category" character varying NOT NULL DEFAULT 'other', "location_found" character varying NULL, "storage_location" character varying NULL, "photo_urls" jsonb NULL, "status" character varying NOT NULL DEFAULT 'stored', "found_by" uuid NOT NULL, "found_at" timestamptz NOT NULL, "guest_name" character varying NULL, "guest_phone" character varying NULL, "guest_email" character varying NULL, "claimed_by_name" character varying NULL, "claimed_notes" character varying NULL, "claimed_at" timestamptz NULL, "disposal_reason" character varying NULL, "disposed_at" timestamptz NULL, "metadata" jsonb NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "lostfounditem_tenant_id_outlet_id" to table: "lost_found_items"
CREATE INDEX "lostfounditem_tenant_id_outlet_id" ON "lost_found_items" ("tenant_id", "outlet_id");
-- Create index "lostfounditem_tenant_id_room_id" to table: "lost_found_items"
CREATE INDEX "lostfounditem_tenant_id_room_id" ON "lost_found_items" ("tenant_id", "room_id");
-- Create index "lostfounditem_tenant_id_status" to table: "lost_found_items"
CREATE INDEX "lostfounditem_tenant_id_status" ON "lost_found_items" ("tenant_id", "status");
