-- Modify "catalog_items" table
ALTER TABLE "catalog_items" ADD COLUMN "requires_prescription" boolean NOT NULL DEFAULT false, ADD COLUMN "is_returnable" boolean NOT NULL DEFAULT true;
-- Create "controlled_substance_logs" table
CREATE TABLE "controlled_substance_logs" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "prescription_id" uuid NULL, "catalog_item_id" uuid NOT NULL, "item_sku" character varying NOT NULL, "item_name" character varying NOT NULL, "quantity_dispensed" double precision NOT NULL, "dispensed_by" uuid NOT NULL, "patient_name" character varying NOT NULL, "patient_id_number" character varying NULL, "witness_staff_id" uuid NULL, "notes" character varying NULL, "dispensed_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "controlledsubstancelog_tenant_id_catalog_item_id" to table: "controlled_substance_logs"
CREATE INDEX "controlledsubstancelog_tenant_id_catalog_item_id" ON "controlled_substance_logs" ("tenant_id", "catalog_item_id");
-- Create index "controlledsubstancelog_tenant_id_dispensed_at" to table: "controlled_substance_logs"
CREATE INDEX "controlledsubstancelog_tenant_id_dispensed_at" ON "controlled_substance_logs" ("tenant_id", "dispensed_at");
-- Create index "controlledsubstancelog_tenant_id_outlet_id" to table: "controlled_substance_logs"
CREATE INDEX "controlledsubstancelog_tenant_id_outlet_id" ON "controlled_substance_logs" ("tenant_id", "outlet_id");
