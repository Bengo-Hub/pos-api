-- Create "print_agents" table
CREATE TABLE "print_agents" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "name" character varying NOT NULL, "key_hash" character varying NOT NULL, "last_seen_at" timestamptz NULL, "version" character varying NULL, "revoked" boolean NOT NULL DEFAULT false, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "print_agents_key_hash_key" to table: "print_agents"
CREATE UNIQUE INDEX "print_agents_key_hash_key" ON "print_agents" ("key_hash");
-- Create index "printagent_tenant_id_outlet_id" to table: "print_agents"
CREATE INDEX "printagent_tenant_id_outlet_id" ON "print_agents" ("tenant_id", "outlet_id");
-- Create "print_jobs" table
CREATE TABLE "print_jobs" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NOT NULL, "order_id" uuid NULL, "job_type" character varying NOT NULL, "profile_id" character varying NULL, "printer_type" character varying NULL, "printer_ip" character varying NULL, "printer_port" bigint NOT NULL DEFAULT 9100, "printer_name" character varying NULL, "paper" character varying NULL, "payload_hex" text NOT NULL, "status" character varying NOT NULL DEFAULT 'queued', "attempts" bigint NOT NULL DEFAULT 0, "claimed_by" character varying NULL, "claim_expires_at" timestamptz NULL, "dedupe_key" character varying NULL, "last_error" text NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "printjob_status_created_at" to table: "print_jobs"
CREATE INDEX "printjob_status_created_at" ON "print_jobs" ("status", "created_at");
-- Create index "printjob_tenant_id_dedupe_key" to table: "print_jobs"
CREATE UNIQUE INDEX "printjob_tenant_id_dedupe_key" ON "print_jobs" ("tenant_id", "dedupe_key");
-- Create index "printjob_tenant_id_outlet_id_status" to table: "print_jobs"
CREATE INDEX "printjob_tenant_id_outlet_id_status" ON "print_jobs" ("tenant_id", "outlet_id", "status");
