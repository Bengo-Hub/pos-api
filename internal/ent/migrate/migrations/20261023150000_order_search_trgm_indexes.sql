-- Trigram indexes for leading-wildcard ILIKE sale search (customer name/phone, invoice number).
-- pg_trgm is already installed on the pos database; IF NOT EXISTS keeps this idempotent.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
-- Create index "posorder_customer_name_trgm" to table: "pos_orders"
CREATE INDEX IF NOT EXISTS "posorder_customer_name_trgm" ON "pos_orders" USING GIN ("customer_name" gin_trgm_ops);
-- Create index "posorder_customer_phone_trgm" to table: "pos_orders"
CREATE INDEX IF NOT EXISTS "posorder_customer_phone_trgm" ON "pos_orders" USING GIN ("customer_phone" gin_trgm_ops);
-- Create index "posorder_order_number_trgm" to table: "pos_orders"
CREATE INDEX IF NOT EXISTS "posorder_order_number_trgm" ON "pos_orders" USING GIN ("order_number" gin_trgm_ops);
