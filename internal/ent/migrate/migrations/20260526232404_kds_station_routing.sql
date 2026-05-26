-- Modify "kds_stations" table
ALTER TABLE "kds_stations" ADD COLUMN "station_type" character varying NOT NULL DEFAULT 'kitchen';
-- Modify "pos_catalog_overrides" table
ALTER TABLE "pos_catalog_overrides" ADD COLUMN "kds_station_id" uuid NULL;
-- Modify "pos_order_lines" table
ALTER TABLE "pos_order_lines" ADD COLUMN "kds_station_id" uuid NULL;
