-- Create "room_folio_payments" table
CREATE TABLE "room_folio_payments" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "room_id" uuid NOT NULL, "room_guest_id" uuid NOT NULL, "amount" double precision NOT NULL, "currency" character varying NOT NULL DEFAULT 'KES', "method" character varying NOT NULL, "reference" character varying NULL, "treasury_intent_id" character varying NULL, "status" character varying NOT NULL DEFAULT 'completed', "recorded_by" uuid NULL, "metadata" jsonb NOT NULL, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "roomfoliopayment_tenant_id_room_guest_id" to table: "room_folio_payments"
CREATE INDEX "roomfoliopayment_tenant_id_room_guest_id" ON "room_folio_payments" ("tenant_id", "room_guest_id");
-- Create index "roomfoliopayment_tenant_id_room_id" to table: "room_folio_payments"
CREATE INDEX "roomfoliopayment_tenant_id_room_id" ON "room_folio_payments" ("tenant_id", "room_id");
