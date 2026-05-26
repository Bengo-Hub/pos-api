-- Modify "pos_device_sessions" table
ALTER TABLE "pos_device_sessions" ADD COLUMN "closing_float" double precision NULL, ADD COLUMN "variance" double precision NULL, ADD COLUMN "notes" character varying NULL;
