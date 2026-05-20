-- Create daily_closings table for end-of-day reconciliation
CREATE TABLE "daily_closings" (
    "id"             uuid          NOT NULL DEFAULT gen_random_uuid(),
    "tenant_id"      uuid          NOT NULL,
    "outlet_id"      uuid          NOT NULL REFERENCES "outlets" ("id") ON DELETE CASCADE,
    "business_date"  timestamptz   NOT NULL,
    "total_sales"    double precision NOT NULL DEFAULT 0,
    "total_refunds"  double precision NOT NULL DEFAULT 0,
    "total_discounts" double precision NOT NULL DEFAULT 0,
    "total_voids"    double precision NOT NULL DEFAULT 0,
    "cash_expected"  double precision NOT NULL DEFAULT 0,
    "cash_actual"    double precision NOT NULL DEFAULT 0,
    "variance"       double precision NOT NULL DEFAULT 0,
    "status"         character varying NOT NULL DEFAULT 'open',
    "closed_by"      uuid          NULL,
    "notes"          character varying NULL,
    "drawer_ids"     jsonb         NOT NULL DEFAULT '[]'::jsonb,
    "created_at"     timestamptz   NOT NULL DEFAULT now(),
    "updated_at"     timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY ("id")
);
CREATE UNIQUE INDEX "daily_closing_tenant_outlet_date" ON "daily_closings" ("tenant_id", "outlet_id", "business_date");

-- Create pos_returns table for structured return/exchange workflow
CREATE TABLE "pos_returns" (
    "id"                   uuid          NOT NULL DEFAULT gen_random_uuid(),
    "tenant_id"            uuid          NOT NULL,
    "outlet_id"            uuid          NOT NULL,
    "order_id"             uuid          NOT NULL,
    "return_number"        character varying NOT NULL,
    "return_type"          character varying NOT NULL DEFAULT 'refund',
    "status"               character varying NOT NULL DEFAULT 'pending',
    "reason"               character varying NULL,
    "refund_amount"        double precision NOT NULL DEFAULT 0,
    "exchange_order_id"    uuid          NULL,
    "requested_by"         uuid          NOT NULL,
    "approved_by"          uuid          NULL,
    "treasury_refund_ref"  character varying NULL,
    "metadata"             jsonb         NOT NULL DEFAULT '{}'::jsonb,
    "created_at"           timestamptz   NOT NULL DEFAULT now(),
    "updated_at"           timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY ("id")
);
CREATE UNIQUE INDEX "pos_return_tenant_return_number" ON "pos_returns" ("tenant_id", "return_number");
CREATE INDEX "pos_return_tenant_order" ON "pos_returns" ("tenant_id", "order_id");

-- Create pos_return_lines table
CREATE TABLE "pos_return_lines" (
    "id"            uuid          NOT NULL DEFAULT gen_random_uuid(),
    "return_id"     uuid          NOT NULL REFERENCES "pos_returns" ("id") ON DELETE CASCADE,
    "order_line_id" uuid          NOT NULL,
    "sku"           character varying NULL,
    "name"          character varying NOT NULL,
    "quantity"      double precision NOT NULL,
    "unit_price"    double precision NOT NULL,
    "total_price"   double precision NOT NULL,
    "reason"        character varying NULL,
    PRIMARY KEY ("id")
);
CREATE INDEX "pos_return_line_return_id" ON "pos_return_lines" ("return_id");
