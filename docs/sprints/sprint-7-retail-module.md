# Sprint 7: Retail Module — pos-api

**Status:** 🔴 Not Started  
**Period:** July–August 2026  
**Last updated:** 2026-05-09  
**Goal:** Extend the POS to support general retail, supermarket, and hardware store operations

---

## Context

Retail businesses differ from hospitality in key ways:
- Sales are predominantly product-scan-and-pay, not table-based or session-based
- Inventory visibility at the point of sale is critical (stock counts, low-stock warnings)
- Weighing scale integration is required for produce, bulk goods, and deli counters
- Serial number capture is mandatory for electronics, appliances, and warranty items
- Layaway (deferred payment plans) is common in hardware and furniture retail
- Customer-facing display mirrors the cart in real time
- Barcode lookup must be sub-second — catalog search by EAN/UPC/internal SKU

The schemas `SerialNumberLog`, `CatalogItem` (with barcode field), `InventorySnapshot`, and `StockAlertSubscription` already exist. This sprint wires those into retail-specific workflows and adds layaway support.

---

## Deliverables

### Barcode & SKU Lookup
- [ ] `GET /{tenant}/pos/catalog/items/lookup?barcode={ean}` — instant barcode lookup endpoint
- [ ] Support EAN-13, EAN-8, UPC-A, QR code, and internal SKU formats
- [ ] Return item with current stock level (from `InventorySnapshot`) and price tier (from `PriceBook`)
- [ ] Cache barcode → item_id mapping in Redis for sub-100ms response

### Weighing Scale Integration
- [ ] `WeighingScaleReading` schema — `id, tenant_id, outlet_id, device_id, weight_grams (int), unit (g|kg|lb), tare_grams (int), read_at`
- [ ] `POST /{tenant}/pos/devices/{device_id}/scale/reading` — receive scale reading from POS terminal
- [ ] `GET /{tenant}/pos/devices/{device_id}/scale/current` — get latest reading (polled by pos-ui)
- [ ] Weight-based line item: `pos_order_lines.weight_grams` (nullable), `price_per_unit` used to compute line total

### Serial Number Capture
- [ ] Wire existing `SerialNumberLog` into order completion flow
- [ ] On order finalize: if `catalog_item.requires_serial = true`, require serial number(s) per unit sold
- [ ] `POST /{tenant}/pos/orders/{order_id}/lines/{line_id}/serials` — attach serial numbers to order line
- [ ] `GET /{tenant}/pos/serials/{serial}` — look up serial number history (sold to whom, when)
- [ ] Validation: serial must be unique per tenant, status: `in_stock → sold`

### Layaway (Deferred Payment)
- [ ] `LayawayPlan` schema — `id, tenant_id, outlet_id, customer_name, customer_phone, pos_order_id (FK), total_amount, amount_paid, balance, due_date, status (active|completed|cancelled|defaulted), notes, created_by, created_at`
- [ ] `LayawayPayment` schema — `id, layaway_plan_id (FK), amount, payment_method, paid_by, paid_at, notes`
- [ ] `POST /{tenant}/pos/orders/{order_id}/layaway` — convert order to layaway (initial deposit)
- [ ] `POST /{tenant}/pos/layaway/{plan_id}/payments` — record instalment payment
- [ ] `GET /{tenant}/pos/layaway` — list plans (filter: status, due_date_before)
- [ ] `GET /{tenant}/pos/layaway/{plan_id}` — plan detail + payment history
- [ ] `PATCH /{tenant}/pos/layaway/{plan_id}/cancel` — cancel plan and restock items
- [ ] On full payment: auto-complete the linked pos_order

### Stock Visibility at POS
- [ ] `GET /{tenant}/pos/catalog/items/{id}/stock` — current stock level from InventorySnapshot + pending orders
- [ ] Low-stock badge on menu grid (stock ≤ threshold)
- [ ] Out-of-stock items: warn but allow override with manager PIN

### Customer Pole Display / Customer-Facing Screen
- [ ] `GET /{tenant}/pos/devices/{device_id}/display` — returns current cart state for customer display
- [ ] Updated on every cart mutation via Redis pub/sub
- [ ] Fields: line items, subtotal, tax, total, payment status

### RBAC Additions
- [ ] New permissions: `pos.retail.view`, `pos.retail.change`, `pos.retail.manage`
- [ ] New permission: `pos.layaway.view`, `pos.layaway.change`, `pos.layaway.manage`
- [ ] New permission: `pos.serial.view`, `pos.serial.manage`
- [ ] Seed new permissions and assign to `pos_admin`, `store_manager`, `cashier`

### Migration
- [ ] Add `WeighingScaleReading` ent schema
- [ ] Add `LayawayPlan` + `LayawayPayment` ent schemas
- [ ] Add `weight_grams` field to `pos_order_lines`
- [ ] Add `requires_serial` bool field to `catalog_items`
- [ ] Run `go generate ./internal/ent`
- [ ] Generate Atlas migration: `retail_module`
- [ ] Update `docs/erd.md` with new entities

---

## Use Cases Covered

| Use Case | Business Types |
|----------|---------------|
| Barcode scan → instant item lookup | Supermarket, hardware, pharmacy, general retail |
| Weight-based pricing (produce, bulk) | Supermarket, deli, butchery, bulk goods |
| Serial number capture at sale | Electronics, appliances, tools, phones |
| Layaway / instalment plan | Furniture, hardware, electronics |
| Real-time stock levels at POS | All retail types |
| Customer-facing display | Supermarket, retail checkout |
