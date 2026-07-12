-- Modify "facilities" table
ALTER TABLE "facilities" ADD COLUMN "booking_mode" character varying NOT NULL DEFAULT 'exclusive';
-- Modify "facility_bookings" table
ALTER TABLE "facility_bookings" ADD COLUMN "outlet_id" uuid NULL, ADD COLUMN "seats" bigint NOT NULL DEFAULT 1, ADD COLUMN "pos_order_id" uuid NULL;
-- Modify "outlet_settings" table
ALTER TABLE "outlet_settings" ADD COLUMN "catalog_use_cases" jsonb NULL;
-- Modify "promotion_rules" table
ALTER TABLE "promotion_rules" ADD COLUMN "get_scope_ids" jsonb NULL, ADD COLUMN "get_pair_map" jsonb NULL;
