-- Modify "room_guests" table
ALTER TABLE "room_guests" ADD COLUMN "late_checkout_approved" boolean NOT NULL DEFAULT false, ADD COLUMN "late_checkout_surcharge" double precision NOT NULL DEFAULT 0;
-- Create "housekeeping_tasks" table
CREATE TABLE "housekeeping_tasks" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "room_guest_id" uuid NULL, "task_type" character varying NOT NULL DEFAULT 'routine_clean', "status" character varying NOT NULL DEFAULT 'pending', "priority" character varying NOT NULL DEFAULT 'normal', "assigned_to" uuid NULL, "notes" character varying NULL, "due_at" timestamptz NULL, "completed_at" timestamptz NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "room_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "housekeeping_tasks_rooms_housekeeping_tasks" FOREIGN KEY ("room_id") REFERENCES "rooms" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "housekeepingtask_tenant_id_assigned_to" to table: "housekeeping_tasks"
CREATE INDEX "housekeepingtask_tenant_id_assigned_to" ON "housekeeping_tasks" ("tenant_id", "assigned_to");
-- Create index "housekeepingtask_tenant_id_room_id" to table: "housekeeping_tasks"
CREATE INDEX "housekeepingtask_tenant_id_room_id" ON "housekeeping_tasks" ("tenant_id", "room_id");
-- Create index "housekeepingtask_tenant_id_status" to table: "housekeeping_tasks"
CREATE INDEX "housekeepingtask_tenant_id_status" ON "housekeeping_tasks" ("tenant_id", "status");
-- Create "room_amenities" table
CREATE TABLE "room_amenities" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "name" character varying NOT NULL, "amenity_type" character varying NOT NULL DEFAULT 'other', "description" character varying NULL, "billing_mode" character varying NOT NULL DEFAULT 'free', "rate" double precision NOT NULL DEFAULT 0, "currency" character varying NOT NULL DEFAULT 'KES', "is_active" boolean NOT NULL DEFAULT true, "metadata" jsonb NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "roomamenity_tenant_id_amenity_type" to table: "room_amenities"
CREATE INDEX "roomamenity_tenant_id_amenity_type" ON "room_amenities" ("tenant_id", "amenity_type");
-- Create index "roomamenity_tenant_id_outlet_id" to table: "room_amenities"
CREATE INDEX "roomamenity_tenant_id_outlet_id" ON "room_amenities" ("tenant_id", "outlet_id");
-- Create "room_amenity_assignments" table
CREATE TABLE "room_amenity_assignments" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "is_included" boolean NOT NULL DEFAULT false, "notes" character varying NULL, "created_at" timestamptz NOT NULL, "room_id" uuid NOT NULL, "amenity_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "room_amenity_assignments_room_amenities_assignments" FOREIGN KEY ("amenity_id") REFERENCES "room_amenities" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION, CONSTRAINT "room_amenity_assignments_rooms_amenity_assignments" FOREIGN KEY ("room_id") REFERENCES "rooms" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "roomamenityassignment_room_id_amenity_id" to table: "room_amenity_assignments"
CREATE UNIQUE INDEX "roomamenityassignment_room_id_amenity_id" ON "room_amenity_assignments" ("room_id", "amenity_id");
-- Create index "roomamenityassignment_tenant_id_room_id" to table: "room_amenity_assignments"
CREATE INDEX "roomamenityassignment_tenant_id_room_id" ON "room_amenity_assignments" ("tenant_id", "room_id");
