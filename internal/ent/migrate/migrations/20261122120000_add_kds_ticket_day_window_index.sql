-- Create index "kdsticket_day_window" to table: "kds_tickets"
CREATE INDEX "kdsticket_day_window" ON "kds_tickets" ("tenant_id", "received_at");
