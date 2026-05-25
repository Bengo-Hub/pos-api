-- Modify "pos_orders" table
ALTER TABLE "pos_orders" ADD COLUMN "covers_count" bigint NOT NULL DEFAULT 0, ADD COLUMN "service_charge_percent" double precision NOT NULL DEFAULT 0, ADD COLUMN "service_charge_amount" double precision NOT NULL DEFAULT 0;
