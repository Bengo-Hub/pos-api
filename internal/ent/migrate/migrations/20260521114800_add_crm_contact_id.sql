-- Modify "appointments" table
ALTER TABLE "appointments" ADD COLUMN "crm_contact_id" uuid NULL;
-- Modify "loyalty_accounts" table
ALTER TABLE "loyalty_accounts" ADD COLUMN "crm_contact_id" uuid NULL;
