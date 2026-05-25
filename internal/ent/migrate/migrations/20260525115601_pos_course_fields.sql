-- Modify "pos_order_lines" table
ALTER TABLE "pos_order_lines" ADD COLUMN "course_number" bigint NOT NULL DEFAULT 0;
-- Modify "pos_orders" table
ALTER TABLE "pos_orders" ADD COLUMN "fired_courses" bigint NOT NULL DEFAULT 0;
