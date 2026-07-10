-- Modify "pos_orders" table
ALTER TABLE "pos_orders" ADD COLUMN "business_date" timestamptz NULL, ADD COLUMN "date_moved_reason" character varying NULL, ADD COLUMN "date_moved_by" uuid NULL, ADD COLUMN "date_moved_at" timestamptz NULL;
