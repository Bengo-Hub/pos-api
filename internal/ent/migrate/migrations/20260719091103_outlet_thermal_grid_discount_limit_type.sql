-- Modify "outlet_settings" table
ALTER TABLE "outlet_settings" ADD COLUMN "discount_limit_type" character varying NULL DEFAULT 'percent';
