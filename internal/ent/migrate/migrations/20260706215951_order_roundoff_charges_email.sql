-- Modify "loyalty_accounts" table
ALTER TABLE "loyalty_accounts" ADD COLUMN "customer_email" character varying NULL;
-- Modify "pos_orders" table
ALTER TABLE "pos_orders" ADD COLUMN "charges_total" double precision NOT NULL DEFAULT 0, ADD COLUMN "round_off" double precision NOT NULL DEFAULT 0;
