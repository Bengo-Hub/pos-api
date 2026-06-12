-- Drop index "table_tenant_id_outlet_id_name" from table: "tables"
DROP INDEX "table_tenant_id_outlet_id_name";
-- Create index "table_tenant_id_outlet_id_section_id_name" to table: "tables"
CREATE UNIQUE INDEX "table_tenant_id_outlet_id_section_id_name" ON "tables" ("tenant_id", "outlet_id", "section_id", "name");
