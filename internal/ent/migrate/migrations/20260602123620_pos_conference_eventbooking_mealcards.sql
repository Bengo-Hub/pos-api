-- Modify "facilities" table
ALTER TABLE "facilities" ADD COLUMN "setup_styles" jsonb NULL, ADD COLUMN "divisible" boolean NOT NULL DEFAULT false, ADD COLUMN "parent_facility_id" uuid NULL;
-- Create "event_bookings" table
CREATE TABLE "event_bookings" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "facility_id" uuid NOT NULL, "inventory_bundle_id" uuid NULL, "event_type" character varying NOT NULL DEFAULT 'conference', "title" character varying NOT NULL, "client_name" character varying NOT NULL, "contact_phone" character varying NULL, "contact_email" character varying NULL, "crm_contact_id" uuid NULL, "start_at" timestamptz NOT NULL, "end_at" timestamptz NOT NULL, "conference_days" bigint NOT NULL DEFAULT 1, "delegate_count" bigint NOT NULL DEFAULT 0, "expected_pax" bigint NOT NULL DEFAULT 0, "guaranteed_minimum_covers" bigint NOT NULL DEFAULT 0, "setup_style" character varying NULL, "deposit_amount" double precision NOT NULL DEFAULT 0, "deposit_refundable" boolean NOT NULL DEFAULT true, "total_amount" double precision NOT NULL DEFAULT 0, "currency" character varying NOT NULL DEFAULT 'KES', "special_requests" text NULL, "master_folio_room_guest_id" uuid NULL, "status" character varying NOT NULL DEFAULT 'confirmed', "created_by" uuid NOT NULL, "metadata" jsonb NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "eventbooking_tenant_id_facility_id" to table: "event_bookings"
CREATE INDEX "eventbooking_tenant_id_facility_id" ON "event_bookings" ("tenant_id", "facility_id");
-- Create index "eventbooking_tenant_id_outlet_id_status" to table: "event_bookings"
CREATE INDEX "eventbooking_tenant_id_outlet_id_status" ON "event_bookings" ("tenant_id", "outlet_id", "status");
-- Create index "eventbooking_tenant_id_start_at" to table: "event_bookings"
CREATE INDEX "eventbooking_tenant_id_start_at" ON "event_bookings" ("tenant_id", "start_at");
-- Create "meal_entitlements" table
CREATE TABLE "meal_entitlements" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "delegate_ref" character varying NULL, "conference_day" timestamptz NOT NULL, "meal_period" character varying NOT NULL, "code" character varying NOT NULL, "valid_window_start" timestamptz NULL, "valid_window_end" timestamptz NULL, "status" character varying NOT NULL DEFAULT 'issued', "redeemed_at" timestamptz NULL, "redeemed_outlet_id" uuid NULL, "redeemed_by" uuid NULL, "pos_order_id" uuid NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "event_booking_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "meal_entitlements_event_bookings_meal_entitlements" FOREIGN KEY ("event_booking_id") REFERENCES "event_bookings" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "mealentitlement_event_booking_id_conference_day_meal_period" to table: "meal_entitlements"
CREATE INDEX "mealentitlement_event_booking_id_conference_day_meal_period" ON "meal_entitlements" ("event_booking_id", "conference_day", "meal_period");
-- Create index "mealentitlement_tenant_id_code" to table: "meal_entitlements"
CREATE UNIQUE INDEX "mealentitlement_tenant_id_code" ON "meal_entitlements" ("tenant_id", "code");
-- Create index "mealentitlement_tenant_id_event_booking_id" to table: "meal_entitlements"
CREATE INDEX "mealentitlement_tenant_id_event_booking_id" ON "meal_entitlements" ("tenant_id", "event_booking_id");
-- Create index "mealentitlement_tenant_id_status" to table: "meal_entitlements"
CREATE INDEX "mealentitlement_tenant_id_status" ON "meal_entitlements" ("tenant_id", "status");
