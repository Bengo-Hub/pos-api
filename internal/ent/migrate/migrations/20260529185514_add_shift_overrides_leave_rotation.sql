-- Create "leave_requests" table
CREATE TABLE "leave_requests" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NULL, "staff_member_id" uuid NOT NULL, "start_date" timestamptz NOT NULL, "end_date" timestamptz NOT NULL, "leave_type" character varying NOT NULL, "reason" character varying NULL, "status" character varying NOT NULL DEFAULT 'pending', "requested_by" uuid NOT NULL, "approved_by" uuid NULL, "rejection_reason" character varying NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "leaverequest_status" to table: "leave_requests"
CREATE INDEX "leaverequest_status" ON "leave_requests" ("status");
-- Create index "leaverequest_tenant_id" to table: "leave_requests"
CREATE INDEX "leaverequest_tenant_id" ON "leave_requests" ("tenant_id");
-- Create index "leaverequest_tenant_id_staff_member_id" to table: "leave_requests"
CREATE INDEX "leaverequest_tenant_id_staff_member_id" ON "leave_requests" ("tenant_id", "staff_member_id");
-- Create "shift_rotations" table
CREATE TABLE "shift_rotations" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NULL, "name" character varying NOT NULL, "cycle_days" bigint NOT NULL DEFAULT 14, "start_date" timestamptz NOT NULL, "is_active" boolean NOT NULL DEFAULT true, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "shiftrotation_tenant_id" to table: "shift_rotations"
CREATE INDEX "shiftrotation_tenant_id" ON "shift_rotations" ("tenant_id");
-- Create index "shiftrotation_tenant_id_is_active" to table: "shift_rotations"
CREATE INDEX "shiftrotation_tenant_id_is_active" ON "shift_rotations" ("tenant_id", "is_active");
-- Create "shift_rotation_slots" table
CREATE TABLE "shift_rotation_slots" ("id" uuid NOT NULL, "rotation_id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "staff_member_id" uuid NOT NULL, "cycle_day" bigint NOT NULL, "start_time" character varying NOT NULL, "end_time" character varying NOT NULL, "is_off_day" boolean NOT NULL DEFAULT false, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "shiftrotationslot_rotation_id" to table: "shift_rotation_slots"
CREATE INDEX "shiftrotationslot_rotation_id" ON "shift_rotation_slots" ("rotation_id");
-- Create index "shiftrotationslot_rotation_id_staff_member_id_cycle_day" to table: "shift_rotation_slots"
CREATE UNIQUE INDEX "shiftrotationslot_rotation_id_staff_member_id_cycle_day" ON "shift_rotation_slots" ("rotation_id", "staff_member_id", "cycle_day");
-- Create index "shiftrotationslot_tenant_id" to table: "shift_rotation_slots"
CREATE INDEX "shiftrotationslot_tenant_id" ON "shift_rotation_slots" ("tenant_id");
-- Create "staff_shift_overrides" table
CREATE TABLE "staff_shift_overrides" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NULL, "staff_member_id" uuid NOT NULL, "date" timestamptz NOT NULL, "override_type" character varying NOT NULL, "start_time" character varying NULL, "end_time" character varying NULL, "reason" character varying NULL, "status" character varying NOT NULL DEFAULT 'approved', "created_by" uuid NOT NULL, "approved_by" uuid NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "staffshiftoverride_date" to table: "staff_shift_overrides"
CREATE INDEX "staffshiftoverride_date" ON "staff_shift_overrides" ("date");
-- Create index "staffshiftoverride_staff_member_id" to table: "staff_shift_overrides"
CREATE INDEX "staffshiftoverride_staff_member_id" ON "staff_shift_overrides" ("staff_member_id");
-- Create index "staffshiftoverride_tenant_id" to table: "staff_shift_overrides"
CREATE INDEX "staffshiftoverride_tenant_id" ON "staff_shift_overrides" ("tenant_id");
-- Create index "staffshiftoverride_tenant_id_staff_member_id_date" to table: "staff_shift_overrides"
CREATE UNIQUE INDEX "staffshiftoverride_tenant_id_staff_member_id_date" ON "staff_shift_overrides" ("tenant_id", "staff_member_id", "date");
