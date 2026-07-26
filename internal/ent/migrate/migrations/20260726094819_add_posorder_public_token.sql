-- Modify "pos_orders" table: add nullable first so existing rows don't violate NOT NULL
-- (ent's Default(uuid.New) is an app-level Go default, not a SQL DEFAULT), backfill every
-- existing row with a fresh random token, then tighten to NOT NULL.
ALTER TABLE "pos_orders" ADD COLUMN "public_token" uuid NULL DEFAULT gen_random_uuid();
UPDATE "pos_orders" SET "public_token" = gen_random_uuid() WHERE "public_token" IS NULL;
ALTER TABLE "pos_orders" ALTER COLUMN "public_token" SET NOT NULL;
-- Create index "pos_orders_public_token_key" to table: "pos_orders"
CREATE UNIQUE INDEX "pos_orders_public_token_key" ON "pos_orders" ("public_token");
