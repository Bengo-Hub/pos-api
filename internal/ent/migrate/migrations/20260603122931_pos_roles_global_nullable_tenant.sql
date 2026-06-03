-- Modify "pos_role_v2s" table
ALTER TABLE "pos_role_v2s" ALTER COLUMN "tenant_id" DROP NOT NULL;
-- Create index "posrolev2_role_code" to table: "pos_role_v2s"
CREATE INDEX "posrolev2_role_code" ON "pos_role_v2s" ("role_code");
