-- Create "referrals" table
CREATE TABLE "referrals" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "referrer_account_id" uuid NOT NULL, "referred_phone" character varying NOT NULL, "referred_account_id" uuid NULL, "code" character varying NOT NULL, "status" character varying NOT NULL DEFAULT 'pending', "bonus_points" bigint NOT NULL DEFAULT 0, "earn_transaction_id" uuid NULL, "created_at" timestamptz NOT NULL, "earned_at" timestamptz NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "referral_referrer_account_id" to table: "referrals"
CREATE INDEX "referral_referrer_account_id" ON "referrals" ("referrer_account_id");
-- Create index "referral_status" to table: "referrals"
CREATE INDEX "referral_status" ON "referrals" ("status");
-- Create index "referral_tenant_id_code" to table: "referrals"
CREATE UNIQUE INDEX "referral_tenant_id_code" ON "referrals" ("tenant_id", "code");
-- Create index "referral_tenant_id_referred_phone" to table: "referrals"
CREATE INDEX "referral_tenant_id_referred_phone" ON "referrals" ("tenant_id", "referred_phone");
