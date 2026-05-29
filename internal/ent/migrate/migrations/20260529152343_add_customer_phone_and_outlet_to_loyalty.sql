-- Modify "loyalty_transactions" table
ALTER TABLE "loyalty_transactions" ADD COLUMN "outlet_id" uuid NULL;
-- Modify "pos_orders" table
ALTER TABLE "pos_orders" ADD COLUMN "customer_phone" character varying NULL, ADD COLUMN "customer_name" character varying NULL;
