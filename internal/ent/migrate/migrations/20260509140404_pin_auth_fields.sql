-- Modify "staff_members" table
ALTER TABLE "staff_members" ADD COLUMN "pin_hash" character varying NULL, ADD COLUMN "pin_failed_attempts" bigint NOT NULL DEFAULT 0, ADD COLUMN "pin_locked_until" timestamptz NULL;
