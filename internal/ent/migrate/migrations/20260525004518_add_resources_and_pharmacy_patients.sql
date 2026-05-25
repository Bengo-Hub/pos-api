-- Create "resources" table
CREATE TABLE "resources" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "name" character varying NOT NULL, "type" character varying NOT NULL DEFAULT 'general', "status" character varying NOT NULL DEFAULT 'available', "notes" character varying NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "resource_tenant_id_outlet_id" to table: "resources"
CREATE INDEX "resource_tenant_id_outlet_id" ON "resources" ("tenant_id", "outlet_id");
-- Create index "resource_tenant_id_outlet_id_status" to table: "resources"
CREATE INDEX "resource_tenant_id_outlet_id_status" ON "resources" ("tenant_id", "outlet_id", "status");
-- Create index "resource_tenant_id_outlet_id_type" to table: "resources"
CREATE INDEX "resource_tenant_id_outlet_id_type" ON "resources" ("tenant_id", "outlet_id", "type");
