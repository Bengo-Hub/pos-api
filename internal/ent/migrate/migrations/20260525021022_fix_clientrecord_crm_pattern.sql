-- Modify "client_records" table
ALTER TABLE "client_records" ADD COLUMN "crm_contact_id" uuid NULL;
-- Create index "clientrecord_tenant_id_crm_contact_id" to table: "client_records"
CREATE INDEX "clientrecord_tenant_id_crm_contact_id" ON "client_records" ("tenant_id", "crm_contact_id");
-- Modify "commission_records" table
ALTER TABLE "commission_records" ADD COLUMN "status" character varying NOT NULL DEFAULT 'pending', ADD COLUMN "notes" character varying NULL;
