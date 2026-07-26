-- Modify "pos_orders" table: add nullable first so existing rows don't violate NOT NULL
-- (ent's Default(uuid.New) is an app-level Go default, not a SQL DEFAULT), backfill every
-- existing row with a fresh random token, then tighten to NOT NULL.
--
-- pos_orders is a live, high-traffic checkout table — a plain ADD COLUMN / UPDATE / SET NOT NULL
-- sequence as three separate statements failed in production 2026-07-26 ("column public_token ...
-- contains null values" on every retry for 2+ hours) because a concurrently-committed order could
-- land between the backfill UPDATE and the NOT NULL enforcement. The backfill + SET NOT NULL are
-- combined into a single DO block under an explicit ACCESS EXCLUSIVE lock so they execute as one
-- atomic unit — no gap for a concurrent insert to slip through, regardless of how the migration
-- runner transaction-wraps the surrounding statements.
ALTER TABLE "pos_orders" ADD COLUMN "public_token" uuid NULL DEFAULT gen_random_uuid();
DO $$
BEGIN
  LOCK TABLE "pos_orders" IN ACCESS EXCLUSIVE MODE;
  UPDATE "pos_orders" SET "public_token" = gen_random_uuid() WHERE "public_token" IS NULL;
  ALTER TABLE "pos_orders" ALTER COLUMN "public_token" SET NOT NULL;
END $$;
-- Create index "pos_orders_public_token_key" to table: "pos_orders"
CREATE UNIQUE INDEX "pos_orders_public_token_key" ON "pos_orders" ("public_token");
