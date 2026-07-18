-- Modify "outlet_settings" table
ALTER TABLE "outlet_settings" ADD COLUMN "allow_price_above_base" boolean NULL DEFAULT true, ADD COLUMN "require_approval_below_base" boolean NULL DEFAULT true;
