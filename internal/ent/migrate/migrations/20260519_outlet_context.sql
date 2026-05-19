-- Add is_hq flag to outlets table for HQ bypass logic
ALTER TABLE "outlets" ADD COLUMN IF NOT EXISTS "is_hq" boolean NOT NULL DEFAULT false;
-- Add outlet-specific terminal settings to outlet_settings
ALTER TABLE "outlet_settings" ADD COLUMN IF NOT EXISTS "pin_login_message" character varying NULL;
ALTER TABLE "outlet_settings" ADD COLUMN IF NOT EXISTS "screensaver_url" character varying NULL;
