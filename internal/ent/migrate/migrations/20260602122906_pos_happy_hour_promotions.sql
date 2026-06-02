-- Modify "promotion_rules" table
ALTER TABLE "promotion_rules" ADD COLUMN "scope_type" character varying NOT NULL DEFAULT 'all', ADD COLUMN "scope_ids" jsonb NULL, ADD COLUMN "discount_type" character varying NOT NULL DEFAULT 'percentage', ADD COLUMN "discount_value" double precision NOT NULL DEFAULT 0, ADD COLUMN "max_discount" double precision NULL;
-- Modify "promotions" table
ALTER TABLE "promotions" ADD COLUMN "outlet_id" uuid NULL, ADD COLUMN "promo_kind" character varying NOT NULL DEFAULT 'code', ADD COLUMN "days_of_week" jsonb NULL, ADD COLUMN "window_start" character varying NULL, ADD COLUMN "window_end" character varying NULL, ADD COLUMN "auto_apply" boolean NOT NULL DEFAULT false;
