-- Modify "staff_members" table
ALTER TABLE "staff_members" ADD COLUMN "employment_type" character varying NULL DEFAULT 'full_time', ADD COLUMN "hourly_rate" double precision NULL, ADD COLUMN "daily_rate" double precision NULL, ADD COLUMN "monthly_salary" double precision NULL, ADD COLUMN "mpesa_phone" character varying NULL, ADD COLUMN "bank_account_number" character varying NULL, ADD COLUMN "bank_name" character varying NULL;
-- Create "staff_advances" table
CREATE TABLE "staff_advances" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "staff_id" uuid NOT NULL, "amount" double precision NOT NULL, "currency" character varying NOT NULL DEFAULT 'KES', "reason" text NULL, "repayment_months" bigint NOT NULL DEFAULT 1, "status" character varying NOT NULL DEFAULT 'active', "approved_by" uuid NULL, "approved_at" timestamptz NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "staffadvance_tenant_id_staff_id" to table: "staff_advances"
CREATE INDEX "staffadvance_tenant_id_staff_id" ON "staff_advances" ("tenant_id", "staff_id");
-- Create index "staffadvance_tenant_id_status" to table: "staff_advances"
CREATE INDEX "staffadvance_tenant_id_status" ON "staff_advances" ("tenant_id", "status");
-- Create "staff_payrolls" table
CREATE TABLE "staff_payrolls" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "staff_id" uuid NOT NULL, "period_start" timestamptz NOT NULL, "period_end" timestamptz NOT NULL, "gross_amount" double precision NOT NULL DEFAULT 0, "total_deductions" double precision NOT NULL DEFAULT 0, "net_amount" double precision NOT NULL DEFAULT 0, "currency" character varying NOT NULL DEFAULT 'KES', "status" character varying NOT NULL DEFAULT 'draft', "approved_by" uuid NULL, "approved_at" timestamptz NULL, "paid_at" timestamptz NULL, "payout_reference" character varying NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "staffpayroll_tenant_id_period_start_period_end" to table: "staff_payrolls"
CREATE INDEX "staffpayroll_tenant_id_period_start_period_end" ON "staff_payrolls" ("tenant_id", "period_start", "period_end");
-- Create index "staffpayroll_tenant_id_staff_id" to table: "staff_payrolls"
CREATE INDEX "staffpayroll_tenant_id_staff_id" ON "staff_payrolls" ("tenant_id", "staff_id");
-- Create index "staffpayroll_tenant_id_status" to table: "staff_payrolls"
CREATE INDEX "staffpayroll_tenant_id_status" ON "staff_payrolls" ("tenant_id", "status");
-- Create "staff_payroll_lines" table
CREATE TABLE "staff_payroll_lines" ("id" uuid NOT NULL, "line_type" character varying NOT NULL, "description" character varying NOT NULL, "amount" double precision NOT NULL, "advance_id" uuid NULL, "payroll_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "staff_payroll_lines_staff_payrolls_lines" FOREIGN KEY ("payroll_id") REFERENCES "staff_payrolls" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "staffpayrollline_payroll_id" to table: "staff_payroll_lines"
CREATE INDEX "staffpayrollline_payroll_id" ON "staff_payroll_lines" ("payroll_id");
