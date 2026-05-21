-- Add shift auto-end settings to outlet_settings table.
ALTER TABLE "outlet_settings"
    ADD COLUMN IF NOT EXISTS "shift_auto_end_enabled" boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS "shift_max_hours" integer NOT NULL DEFAULT 12;
