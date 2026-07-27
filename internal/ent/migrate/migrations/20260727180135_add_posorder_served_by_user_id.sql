-- Modify "pos_orders" table
ALTER TABLE "pos_orders" ALTER COLUMN "public_token" DROP DEFAULT, ADD COLUMN "served_by_user_id" uuid NULL;
