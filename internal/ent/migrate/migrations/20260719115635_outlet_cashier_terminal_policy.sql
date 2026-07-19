-- Modify "outlet_settings" table
ALTER TABLE "outlet_settings" ADD COLUMN "cashier_sales_visibility" character varying NULL, ADD COLUMN "auto_logout_after_sale" boolean NULL, ADD COLUMN "cashier_terminal_surface" character varying NULL;
