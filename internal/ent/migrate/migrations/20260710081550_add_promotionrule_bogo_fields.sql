-- Modify "promotion_rules" table
ALTER TABLE "promotion_rules" ADD COLUMN "buy_quantity" bigint NOT NULL DEFAULT 1, ADD COLUMN "get_quantity" bigint NOT NULL DEFAULT 1, ADD COLUMN "get_discount_percent" double precision NOT NULL DEFAULT 100;
