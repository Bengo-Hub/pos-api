-- Modify "staff_members" table
ALTER TABLE "staff_members" ADD COLUMN "pin_fast_hash" character varying NULL;
-- Create index "staffmember_tenant_id_outlet_id_pin_fast_hash" to table: "staff_members"
CREATE UNIQUE INDEX "staffmember_tenant_id_outlet_id_pin_fast_hash" ON "staff_members" ("tenant_id", "outlet_id", "pin_fast_hash");
