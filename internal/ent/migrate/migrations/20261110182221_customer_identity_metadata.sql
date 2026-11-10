-- Modify "client_records" table
ALTER TABLE "client_records" ADD COLUMN "metadata" jsonb NULL;
-- Modify "loyalty_accounts" table
ALTER TABLE "loyalty_accounts" ADD COLUMN "metadata" jsonb NULL;
