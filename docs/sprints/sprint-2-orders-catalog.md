# Sprint 2: Orders, Catalog, Tables & Payments — pos-api

**Status:** ✅ Complete  
**Period:** February–March 2026  
**Last updated:** 2026-05-09  
**Goal:** Core POS transaction flow — catalog, order lifecycle, payment recording, table management, cash drawer, bar tabs, promotions

---

## Deliverables

### Catalog
- [x] `CatalogItem` entity — mirror of inventory-service product master (`external_item_id`, `source_service`, `barcode`, `modifier_schema`, `synced_at`)
- [x] `PriceBook` + `PriceBookItem` — channel-scoped pricing (happy hour, wholesale, etc.)
- [x] `ModifierGroup` + `Modifier` — item customisation (toppings, sides, sizes)
- [x] Catalog CRUD endpoints (`/{tenant}/pos/catalog/items`, `/{tenant}/pos/catalog/categories`)
- [x] 48 catalog items seeded from inventory-api master data

### Orders
- [x] `POSOrder` entity — order header (order_number, status state-machine, subtotal, tax_total, discount_total, total_amount, currency)
- [x] `POSOrderLine` entity — line items (catalog_item_id, sku, name, quantity, unit_price, total_price)
- [x] `POSLineModifier` entity — applied modifiers per line
- [x] `POSOrderEvent` entity — audit log (status changes, voids, discounts)
- [x] Order CRUD endpoints (`POST`, `GET`, `PUT /status`)
- [x] Tax calculation (env: `TAX_RATE_PERCENT`)
- [x] Discount calculation (percentage / fixed with max cap)
- [x] Order number generation (env: `ORDER_PREFIX`)
- [x] `orders.Service` with state machine: `draft → open → completed/cancelled/voided → refunded`

### Payments
- [x] `Tender` entity — payment types (cash, card, M-Pesa, loyalty, room_charge)
- [x] `POSPayment` entity — payment records (amount, tender_id, payment_status, provider_reference)
- [x] `POSRefund` entity — refund records
- [x] `payments.Service` — auto-completes order when fully paid
- [x] 4 tenders seeded (cash, card, M-Pesa, loyalty)
- [x] `POST /{tenant}/pos/orders/{id}/payments` — record payment
- [x] `GET /{tenant}/pos/orders/{id}/payments` — list payments

### Tables & Sections
- [x] `Section` entity — floor plan sections (main_hall, outdoor, bar, vip, vvip, rooftop)
- [x] `Table` entity — table definitions with spatial layout (x_position, y_position, table_type, tags, section_id)
- [x] `TableAssignment` entity — table ↔ order linkage
- [x] Table management endpoints (`GET`, `POST /assign`, `POST /release`)
- [x] 3 sections + 12 tables seeded (Indoor, Outdoor, Bar with VIP/VVIP tags)

### Cash Drawer
- [x] `CashDrawer` entity — drawer lifecycle (opening_float, closing_amount, variance_amount, status)
- [x] `CashDrawerEvent` entity — skims, drops, shortages, audits
- [x] Cash drawer endpoints (`POST /open`, `POST /close`, `GET /current`)

### Bar Tabs
- [x] `BarTab` entity — tab lifecycle (tab_code, customer_name, limit_amount, status)
- [x] `BarTabEvent` entity — tab events
- [x] Bar tab endpoints

### Promotions & Gift Cards
- [x] `Promotion` + `PromotionRule` + `PromotionApplication` entities
- [x] `GiftCard` + `GiftCardTransaction` entities
- [x] `promotions.Service` — promo code validation (percentage/fixed with max cap)

### Inventory Touchpoints
- [x] `StockConsumptionEvent` entity — emitted on sale finalized
- [x] `StockAlertSubscription` entity — user-configured low-stock alerts
- [x] `InventorySnapshot` entity — read-only cached inventory view

### Events Published
- `pos.sale.finalized` — triggers inventory backflush and treasury ledger update
- `pos.drawer.closed` — reports end-of-shift cash position to treasury
- `pos.menu.updated` — signals ordering-backend to refresh online catalog

### Events Consumed
- `inventory.catalog.updated` — refresh `catalog_items` projection
- `inventory.stock.low` — create stock alert notification

---

## New Deliverables (2026-05-09)

### Treasury Payment Intent Flow
- [x] `POST /{tenant}/pos/orders/{id}/payments/intent` — creates treasury intent; cash settles immediately, digital returns `{payment_intent_id, initiate_url}`
- [x] `POST /{tenant}/pos/payments/initiate` — proxy to treasury `InitiateIntent` (the `initiate_url` target)
- [x] `internal/modules/treasury/client.go` — S2S client using `INTERNAL_SERVICE_KEY`
- [x] `payments.Service.CreatePaymentIntent()` — cash path settles locally; digital delegates to treasury-api
- [x] `payments.Service.ConfirmPaymentByIntentID()` / `FailPaymentByIntentID()` — called by NATS subscribers
- [x] `internal/modules/payments/treasury_subscriber.go` — subscribes to `treasury.payment.success`, `treasury.payment.failed`, `treasury.etims.invoice_transmitted`
- [x] eTIMS fields on `pos_orders`: `etims_invoice_number`, `etims_qr_code_url` + Atlas migration `20260509092640_etims_fields.sql`

## Pending / Carry-forward
- [ ] `pos.sale.finalized` — event not yet published after order auto-completion (wired in payments.Service but publish call missing)
- [ ] `pos.sale.finalized` → inventory-api `POST /consumption` HTTP call not yet wired (Sprint 6)
- [ ] `inventory.catalog.updated` NATS subscriber not yet implemented (Sprint 6)
- [ ] KDS endpoints not yet added (Sprint 4)
