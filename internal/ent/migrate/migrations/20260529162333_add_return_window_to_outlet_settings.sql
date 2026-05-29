-- Modify "outlet_settings" table
ALTER TABLE "outlet_settings" ADD COLUMN "return_window_days" bigint NULL DEFAULT 30;
