-- Create "room_damage_reports" table
CREATE TABLE "room_damage_reports" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "room_id" uuid NOT NULL, "room_guest_id" uuid NULL, "description" character varying NOT NULL, "amount" double precision NOT NULL, "currency" character varying NOT NULL DEFAULT 'KES', "evidence_urls" jsonb NULL, "status" character varying NOT NULL DEFAULT 'pending', "reported_by" uuid NOT NULL, "reviewed_by" uuid NULL, "reviewed_at" timestamptz NULL, "review_notes" character varying NULL, "folio_item_id" uuid NULL, "metadata" jsonb NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "roomdamagereport_tenant_id_room_guest_id" to table: "room_damage_reports"
CREATE INDEX "roomdamagereport_tenant_id_room_guest_id" ON "room_damage_reports" ("tenant_id", "room_guest_id");
-- Create index "roomdamagereport_tenant_id_room_id" to table: "room_damage_reports"
CREATE INDEX "roomdamagereport_tenant_id_room_id" ON "room_damage_reports" ("tenant_id", "room_id");
-- Create index "roomdamagereport_tenant_id_status" to table: "room_damage_reports"
CREATE INDEX "roomdamagereport_tenant_id_status" ON "room_damage_reports" ("tenant_id", "status");
