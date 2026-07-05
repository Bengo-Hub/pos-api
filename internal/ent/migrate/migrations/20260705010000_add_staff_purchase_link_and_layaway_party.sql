-- Staff fund-from-salary: layaway/credit sales can be for a staff member and funded via ERP payroll
-- deduction. Add party fields to layaway_plans + a local StaffPurchaseLink that tracks the ERP
-- recoverable + settlement as payroll recovers it.
ALTER TABLE "layaway_plans" ADD COLUMN "party_type" character varying NOT NULL DEFAULT 'customer';
ALTER TABLE "layaway_plans" ADD COLUMN "staff_member_id" uuid NULL;
ALTER TABLE "layaway_plans" ADD COLUMN "loyalty_account_id" uuid NULL;
ALTER TABLE "layaway_plans" ADD COLUMN "fund_from_salary" boolean NOT NULL DEFAULT false;

CREATE TABLE "staff_purchase_links" (
  "id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "outlet_id" uuid NULL,
  "staff_member_id" uuid NOT NULL,
  "user_id" uuid NOT NULL,
  "origin" character varying NOT NULL,
  "layaway_plan_id" uuid NULL,
  "pos_order_id" uuid NULL,
  "source_key" character varying NOT NULL,
  "erp_purchase_id" uuid NULL,
  "principal" double precision NOT NULL,
  "amount_settled" double precision NOT NULL,
  "outstanding" double precision NOT NULL,
  "sync_status" character varying NOT NULL DEFAULT 'pending',
  "status" character varying NOT NULL DEFAULT 'active',
  "sync_error" character varying NULL,
  "created_at" timestamptz NOT NULL,
  "updated_at" timestamptz NOT NULL,
  PRIMARY KEY ("id")
);
CREATE INDEX "staffpurchaselink_tenant_id" ON "staff_purchase_links" ("tenant_id");
CREATE INDEX "staffpurchaselink_tenant_id_staff_member_id" ON "staff_purchase_links" ("tenant_id", "staff_member_id");
CREATE UNIQUE INDEX "staffpurchaselink_tenant_id_source_key" ON "staff_purchase_links" ("tenant_id", "source_key");
