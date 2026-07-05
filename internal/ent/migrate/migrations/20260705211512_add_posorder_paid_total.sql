-- Modify "pos_orders" table
ALTER TABLE "pos_orders" ADD COLUMN "paid_total" double precision NOT NULL DEFAULT 0;
-- Backfill: initialize paid_total from the sum of each order's COMPLETED payments so the
-- paid/partial/due filter and badges are correct for historical orders.
UPDATE "pos_orders" SET "paid_total" = COALESCE((SELECT SUM(p."amount") FROM "pos_payments" p WHERE p."order_id" = "pos_orders"."id" AND p."status" = 'completed'), 0);
