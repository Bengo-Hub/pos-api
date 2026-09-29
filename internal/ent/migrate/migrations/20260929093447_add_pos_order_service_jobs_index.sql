-- Create index "posorder_service_jobs" to table: "pos_orders"
CREATE INDEX "posorder_service_jobs" ON "pos_orders" ("tenant_id", "outlet_id", "status", "created_at") WHERE ((order_subtype)::text = 'service_job'::text);
