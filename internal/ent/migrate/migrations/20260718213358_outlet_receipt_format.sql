-- Modify "outlet_settings" table
ALTER TABLE "outlet_settings" ADD COLUMN "receipt_format" character varying NULL DEFAULT 'auto';
