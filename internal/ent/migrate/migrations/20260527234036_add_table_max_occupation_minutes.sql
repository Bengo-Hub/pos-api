-- Modify "outlet_settings" table
ALTER TABLE "outlet_settings" ADD COLUMN "table_max_occupation_minutes" bigint NULL DEFAULT 240;
