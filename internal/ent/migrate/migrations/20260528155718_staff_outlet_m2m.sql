-- Create "staff_outlets" join table
CREATE TABLE "staff_outlets" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "is_home_outlet" boolean NOT NULL DEFAULT false, "assigned_at" timestamptz NOT NULL, "outlet_id" uuid NOT NULL, "staff_member_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "staff_outlets_outlets_staff_outlets" FOREIGN KEY ("outlet_id") REFERENCES "outlets" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION, CONSTRAINT "staff_outlets_staff_members_outlets" FOREIGN KEY ("staff_member_id") REFERENCES "staff_members" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "staffoutlet_staff_member_id_outlet_id" to table: "staff_outlets"
CREATE UNIQUE INDEX "staffoutlet_staff_member_id_outlet_id" ON "staff_outlets" ("staff_member_id", "outlet_id");
-- Create index "staffoutlet_tenant_id_outlet_id" to table: "staff_outlets"
CREATE INDEX "staffoutlet_tenant_id_outlet_id" ON "staff_outlets" ("tenant_id", "outlet_id");
-- Migrate existing outlet_id assignments to the new join table (is_home_outlet=true for all)
INSERT INTO "staff_outlets" ("id", "tenant_id", "outlet_id", "staff_member_id", "is_home_outlet", "assigned_at")
SELECT gen_random_uuid(), sm."tenant_id", sm."outlet_id", sm."id", true, sm."created_at"
FROM "staff_members" sm WHERE sm."outlet_id" IS NOT NULL;
-- Drop old unique index before dropping outlet_id column
DROP INDEX IF EXISTS "staffmember_tenant_id_outlet_id_pin_fast_hash";
-- Modify "staff_members" table
ALTER TABLE "staff_members" DROP COLUMN "outlet_id";
-- Create index "staffmember_tenant_id_pin_fast_hash" to table: "staff_members"
CREATE UNIQUE INDEX "staffmember_tenant_id_pin_fast_hash" ON "staff_members" ("tenant_id", "pin_fast_hash");
