-- Modify "pos_order_lines" table
ALTER TABLE "pos_order_lines" ADD COLUMN "serial_number" character varying NULL, ADD COLUMN "partial_units" double precision NULL;
