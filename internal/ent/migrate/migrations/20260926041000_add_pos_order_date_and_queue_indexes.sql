-- Create index "posorder_tenant_id_created_at" to table: "pos_orders"
CREATE INDEX "posorder_tenant_id_created_at" ON "pos_orders" ("tenant_id", "created_at");
-- Create index "posorder_tenant_id_business_date" to table: "pos_orders"
CREATE INDEX "posorder_tenant_id_business_date" ON "pos_orders" ("tenant_id", "business_date") WHERE (business_date IS NOT NULL);
-- Create index "posorder_tenant_id_offline_created_at" to table: "pos_orders"
CREATE INDEX "posorder_tenant_id_offline_created_at" ON "pos_orders" ("tenant_id", "offline_created_at") WHERE (offline_created_at IS NOT NULL);
-- Create index "posorder_pickup_queue" to table: "pos_orders"
CREATE INDEX "posorder_pickup_queue" ON "pos_orders" ("tenant_id", "created_at") WHERE (((status)::text <> ALL ((ARRAY['cancelled'::character varying, 'voided'::character varying])::text[])) AND ((metadata ->> 'collected'::text) IS DISTINCT FROM 'true'::text));
