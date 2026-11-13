-- Create index "loyaltyaccount_crm_contact_id" to table: "loyalty_accounts"
CREATE INDEX "loyaltyaccount_crm_contact_id" ON "loyalty_accounts" ("crm_contact_id");
-- Create index "roomfolioitem_tenant_id_pos_order_id" to table: "room_folio_items"
CREATE INDEX "roomfolioitem_tenant_id_pos_order_id" ON "room_folio_items" ("tenant_id", "pos_order_id");
