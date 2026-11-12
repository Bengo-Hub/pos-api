-- Create index "roomfoliopayment_return_credit_ref" to table: "room_folio_payments"
CREATE UNIQUE INDEX "roomfoliopayment_return_credit_ref" ON "room_folio_payments" ("tenant_id", "room_guest_id", "reference") WHERE ((method)::text = 'return_credit'::text);
