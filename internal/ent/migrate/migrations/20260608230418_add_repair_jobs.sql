-- Create "repair_jobs" table
CREATE TABLE "repair_jobs" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "outlet_id" uuid NULL, "job_number" character varying NOT NULL, "customer_phone" character varying NULL, "customer_name" character varying NULL, "device_description" character varying NULL, "reported_issue" text NULL, "status" character varying NOT NULL DEFAULT 'intake', "diagnosis" character varying NULL, "estimated_cost" double precision NOT NULL, "quoted_cost" double precision NULL, "assigned_staff_id" uuid NULL, "pos_order_id" uuid NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "repairjob_tenant_id_job_number" to table: "repair_jobs"
CREATE UNIQUE INDEX "repairjob_tenant_id_job_number" ON "repair_jobs" ("tenant_id", "job_number");
-- Create index "repairjob_tenant_id_status" to table: "repair_jobs"
CREATE INDEX "repairjob_tenant_id_status" ON "repair_jobs" ("tenant_id", "status");
-- Create "repair_job_events" table
CREATE TABLE "repair_job_events" ("id" uuid NOT NULL, "event_type" character varying NOT NULL, "notes" text NULL, "actor_id" uuid NULL, "created_at" timestamptz NOT NULL, "repair_job_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "repair_job_events_repair_jobs_events" FOREIGN KEY ("repair_job_id") REFERENCES "repair_jobs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "repairjobevent_repair_job_id" to table: "repair_job_events"
CREATE INDEX "repairjobevent_repair_job_id" ON "repair_job_events" ("repair_job_id");
-- Create "repair_job_parts" table
CREATE TABLE "repair_job_parts" ("id" uuid NOT NULL, "inventory_sku" character varying NULL, "inventory_item_id" uuid NULL, "description" character varying NULL, "quantity" double precision NOT NULL DEFAULT 1, "unit_cost" double precision NOT NULL, "line_total" double precision NOT NULL, "repair_job_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "repair_job_parts_repair_jobs_parts" FOREIGN KEY ("repair_job_id") REFERENCES "repair_jobs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "repairjobpart_repair_job_id" to table: "repair_job_parts"
CREATE INDEX "repairjobpart_repair_job_id" ON "repair_job_parts" ("repair_job_id");
