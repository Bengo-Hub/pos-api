-- Modify "pos_orders" table
ALTER TABLE "pos_orders" ADD COLUMN "etims_invoice_number" character varying NULL, ADD COLUMN "etims_qr_code_url" character varying NULL;
