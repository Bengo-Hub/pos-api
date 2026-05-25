-- Modify "client_records" table
ALTER TABLE "client_records" DROP COLUMN "full_name", DROP COLUMN "email", DROP COLUMN "date_of_birth", DROP COLUMN "gender";
-- Modify "webhook_subscriptions" table
ALTER TABLE "webhook_subscriptions" DROP COLUMN "url", DROP COLUMN "secret_key", DROP COLUMN "status";
