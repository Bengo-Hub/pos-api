-- Create "sections" table
CREATE TABLE "sections" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "name" character varying NOT NULL, "slug" character varying NOT NULL, "description" text NULL, "floor_number" bigint NOT NULL DEFAULT 1, "sort_order" bigint NOT NULL DEFAULT 0, "is_active" boolean NOT NULL DEFAULT true, "section_type" character varying NOT NULL DEFAULT 'main_hall', "metadata" jsonb NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "section_is_active" to table: "sections"
CREATE INDEX "section_is_active" ON "sections" ("is_active");
-- Create index "section_tenant_id_outlet_id" to table: "sections"
CREATE INDEX "section_tenant_id_outlet_id" ON "sections" ("tenant_id", "outlet_id");
-- Create index "section_tenant_id_outlet_id_slug" to table: "sections"
CREATE UNIQUE INDEX "section_tenant_id_outlet_id_slug" ON "sections" ("tenant_id", "outlet_id", "slug");
-- Modify "tables" table
ALTER TABLE "tables" ADD COLUMN "table_type" character varying NOT NULL DEFAULT 'standard', ADD COLUMN "x_position" double precision NULL, ADD COLUMN "y_position" double precision NULL, ADD COLUMN "tags" jsonb NULL, ADD COLUMN "section_id" uuid NULL, ADD CONSTRAINT "tables_sections_tables" FOREIGN KEY ("section_id") REFERENCES "sections" ("id") ON UPDATE NO ACTION ON DELETE SET NULL;
