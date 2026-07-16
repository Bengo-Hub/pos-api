-- Modify "pos_order_lines" table
ALTER TABLE "pos_order_lines" ADD COLUMN "created_at" timestamptz NULL;
-- Modify "pos_orders" table
ALTER TABLE "pos_orders" ADD COLUMN "etims_scu_id" character varying NULL, ADD COLUMN "etims_cu_inv_no" character varying NULL, ADD COLUMN "etims_rcpt_sign" character varying NULL, ADD COLUMN "etims_kra_pin" character varying NULL;
