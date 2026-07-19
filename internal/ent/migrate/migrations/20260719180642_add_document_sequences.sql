-- Create "document_sequences" table
CREATE TABLE "document_sequences" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "doc_type" character varying NOT NULL, "prefix" character varying NULL, "separator" character varying NOT NULL DEFAULT '-', "date_format" character varying NULL, "padding" bigint NOT NULL DEFAULT 6, "reset_freq" character varying NOT NULL DEFAULT 'never', "current_val" bigint NOT NULL DEFAULT 0, "last_reset" timestamptz NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "documentsequence_tenant_id_doc_type" to table: "document_sequences"
CREATE UNIQUE INDEX "documentsequence_tenant_id_doc_type" ON "document_sequences" ("tenant_id", "doc_type");
