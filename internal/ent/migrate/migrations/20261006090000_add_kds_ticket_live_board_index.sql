-- Create index "kdsticket_live_board" to table: "kds_tickets"
CREATE INDEX "kdsticket_live_board" ON "kds_tickets" ("tenant_id", "received_at") WHERE (status IN ('pending', 'in_progress', 'ready'));
