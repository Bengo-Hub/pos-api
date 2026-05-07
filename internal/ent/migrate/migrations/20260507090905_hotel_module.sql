-- Modify "outbox_events" table
ALTER TABLE "outbox_events" ALTER COLUMN "aggregate_type" DROP DEFAULT, ALTER COLUMN "aggregate_id" DROP DEFAULT;
-- Modify "pos_orders" table
ALTER TABLE "pos_orders" ADD COLUMN "order_subtype" character varying NOT NULL DEFAULT 'dine_in', ADD COLUMN "room_id" uuid NULL, ADD COLUMN "room_guest_id" uuid NULL;
-- Create "facilities" table
CREATE TABLE "facilities" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "name" character varying NOT NULL, "facility_type" character varying NOT NULL DEFAULT 'other', "capacity" bigint NOT NULL DEFAULT 0, "rate_per_session" double precision NOT NULL, "currency" character varying NOT NULL DEFAULT 'KES', "opening_time" character varying NOT NULL DEFAULT '06:00', "closing_time" character varying NOT NULL DEFAULT '22:00', "status" character varying NOT NULL DEFAULT 'available', "is_active" boolean NOT NULL DEFAULT true, "metadata" jsonb NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "facility_tenant_id_outlet_id" to table: "facilities"
CREATE INDEX "facility_tenant_id_outlet_id" ON "facilities" ("tenant_id", "outlet_id");
-- Create index "facility_tenant_id_status" to table: "facilities"
CREATE INDEX "facility_tenant_id_status" ON "facilities" ("tenant_id", "status");
-- Create "facility_bookings" table
CREATE TABLE "facility_bookings" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "room_guest_id" uuid NULL, "guest_name" character varying NOT NULL, "phone" character varying NOT NULL, "session_date" timestamptz NOT NULL, "start_time" character varying NOT NULL, "end_time" character varying NOT NULL, "guests_count" bigint NOT NULL DEFAULT 1, "amount" double precision NOT NULL, "currency" character varying NOT NULL DEFAULT 'KES', "status" character varying NOT NULL DEFAULT 'confirmed', "booked_by" uuid NOT NULL, "notes" character varying NULL, "metadata" jsonb NOT NULL, "created_at" timestamptz NOT NULL, "facility_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "facility_bookings_facilities_bookings" FOREIGN KEY ("facility_id") REFERENCES "facilities" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "facilitybooking_tenant_id_facility_id" to table: "facility_bookings"
CREATE INDEX "facilitybooking_tenant_id_facility_id" ON "facility_bookings" ("tenant_id", "facility_id");
-- Create index "facilitybooking_tenant_id_session_date_status" to table: "facility_bookings"
CREATE INDEX "facilitybooking_tenant_id_session_date_status" ON "facility_bookings" ("tenant_id", "session_date", "status");
-- Create "rooms" table
CREATE TABLE "rooms" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "room_number" character varying NOT NULL, "name" character varying NOT NULL, "room_type" character varying NOT NULL DEFAULT 'standard', "floor" bigint NOT NULL DEFAULT 1, "rate_per_night" double precision NOT NULL, "currency" character varying NOT NULL DEFAULT 'KES', "status" character varying NOT NULL DEFAULT 'available', "is_active" boolean NOT NULL DEFAULT true, "metadata" jsonb NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "room_tenant_id_outlet_id" to table: "rooms"
CREATE INDEX "room_tenant_id_outlet_id" ON "rooms" ("tenant_id", "outlet_id");
-- Create index "room_tenant_id_room_number" to table: "rooms"
CREATE UNIQUE INDEX "room_tenant_id_room_number" ON "rooms" ("tenant_id", "room_number");
-- Create index "room_tenant_id_status" to table: "rooms"
CREATE INDEX "room_tenant_id_status" ON "rooms" ("tenant_id", "status");
-- Create "room_guests" table
CREATE TABLE "room_guests" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "guest_name" character varying NOT NULL, "phone" character varying NOT NULL, "id_number" character varying NOT NULL, "check_in_date" timestamptz NOT NULL, "nights" bigint NOT NULL, "check_out_date" timestamptz NOT NULL, "total_room_charge" double precision NOT NULL, "status" character varying NOT NULL DEFAULT 'active', "checked_in_by" uuid NOT NULL, "checked_out_by" uuid NULL, "checked_in_at" timestamptz NOT NULL, "checked_out_at" timestamptz NULL, "metadata" jsonb NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "room_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "room_guests_rooms_guests" FOREIGN KEY ("room_id") REFERENCES "rooms" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "roomguest_tenant_id_room_id" to table: "room_guests"
CREATE INDEX "roomguest_tenant_id_room_id" ON "room_guests" ("tenant_id", "room_id");
-- Create index "roomguest_tenant_id_status" to table: "room_guests"
CREATE INDEX "roomguest_tenant_id_status" ON "room_guests" ("tenant_id", "status");
-- Create "room_folio_items" table
CREATE TABLE "room_folio_items" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "description" character varying NOT NULL, "amount" double precision NOT NULL, "currency" character varying NOT NULL DEFAULT 'KES', "charge_type" character varying NOT NULL DEFAULT 'other', "pos_order_id" uuid NULL, "created_by" uuid NOT NULL, "metadata" jsonb NOT NULL, "created_at" timestamptz NOT NULL, "room_id" uuid NOT NULL, "room_guest_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "room_folio_items_room_guests_folio_items" FOREIGN KEY ("room_guest_id") REFERENCES "room_guests" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION, CONSTRAINT "room_folio_items_rooms_folio_items" FOREIGN KEY ("room_id") REFERENCES "rooms" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "roomfolioitem_tenant_id_room_guest_id" to table: "room_folio_items"
CREATE INDEX "roomfolioitem_tenant_id_room_guest_id" ON "room_folio_items" ("tenant_id", "room_guest_id");
-- Create index "roomfolioitem_tenant_id_room_id" to table: "room_folio_items"
CREATE INDEX "roomfolioitem_tenant_id_room_id" ON "room_folio_items" ("tenant_id", "room_id");
