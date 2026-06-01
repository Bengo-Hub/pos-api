-- Modify "outlet_settings" table
ALTER TABLE "outlet_settings" ADD COLUMN "mpesa_paybill" character varying NULL;
ALTER TABLE "outlet_settings" ADD COLUMN "mpesa_account_reference" character varying NULL;
ALTER TABLE "outlet_settings" ADD COLUMN "airtel_money_number" character varying NULL;
ALTER TABLE "outlet_settings" ADD COLUMN "bank_name" character varying NULL;
ALTER TABLE "outlet_settings" ADD COLUMN "bank_account_number" character varying NULL;
ALTER TABLE "outlet_settings" ADD COLUMN "bank_account_name" character varying NULL;
ALTER TABLE "outlet_settings" ADD COLUMN "show_payment_info_on_receipt" boolean NULL DEFAULT false;
