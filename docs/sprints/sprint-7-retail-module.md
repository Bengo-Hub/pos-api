# Sprint 7: Retail Module — pos-api

**Status:** ✅ Core Delivered — barcode lookup, scale, layaway, serial/lot fields shipped; stock visibility and pole display pending  
**Period:** July–August 2026  
**Last updated:** 2026-05-21  
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
- [x] `GET /{tenant}/pos/catalog/barcode/{barcode}` — barcode lookup endpoint (`catalog_barcode.go` handler)
- [x] Return item by barcode field on `CatalogItem`
- [ ] Redis cache for sub-100ms response (not confirmed)
- [ ] Stock level included in barcode lookup response (not confirmed)

### Weighing Scale Integration
- [x] `WeighingScaleReading` schema (`internal/ent/schema/weighingscalereading.go`)
- [x] `POST /{tenant}/pos/scale/readings` — receive scale reading (`scale.go` handler)
- [x] `GET /{tenant}/pos/scale/readings` — list readings (polled by pos-ui)
- [ ] `GET /{tenant}/pos/devices/{device_id}/scale/current` — device-specific current reading (not wired; list endpoint used instead)
- [x] `pos_order_lines.weight_grams` field on `POSOrderLine` schema

### Serial Number Capture
- [x] `SerialNumberLog` schema exists (`internal/ent/schema/serialnumberlog.go`)
- [x] `serial_number` and `lot_number` fields on `pos_order_lines` (via `POSOrderLine` schema)
- [ ] `POST /{tenant}/pos/orders/{order_id}/lines/{line_id}/serials` endpoint — not yet registered
- [ ] Serial validation and `in_stock → sold` state machine — not wired

### Layaway (Deferred Payment)
- [x] `LayawayPlan` schema (`internal/ent/schema/layawayplan.go`)
- [x] `LayawayPayment` schema (`internal/ent/schema/layawaypayment.go`)
- [x] `POST /{tenant}/pos/layaways` — create layaway
- [x] `GET /{tenant}/pos/layaways` — list plans
- [x] `GET /{tenant}/pos/layaways/{id}` — plan detail
- [x] `POST /{tenant}/pos/layaways/{id}/payments` — record instalment
- [x] `POST /{tenant}/pos/layaways/{id}/cancel` — cancel plan
- [ ] Auto-complete linked pos_order on full payment — not confirmed wired

### Stock Visibility at POS
- [ ] `GET /{tenant}/pos/catalog/items/{id}/stock` endpoint — not implemented
- [ ] Low-stock badge on menu grid — not implemented (pos-ui side)
- [ ] Out-of-stock override with manager PIN — not implemented

### Customer Pole Display / Customer-Facing Screen
- [ ] `GET /{tenant}/pos/devices/{device_id}/display` — not implemented
- [ ] Redis pub/sub for real-time cart — not implemented

### RBAC Additions
- [ ] `pos.retail.*`, `pos.layaway.*`, `pos.serial.*` permissions — not yet seeded

### Migration
- [x] `WeighingScaleReading` ent schema added
- [x] `LayawayPlan` + `LayawayPayment` ent schemas added
- [x] `weight_grams`, `serial_number`, `lot_number` fields on `pos_order_lines`
- [x] `barcode`, `requires_serial`, `weight_based` fields on `catalog_items` (via `catalogitem.go`)
- [x] Atlas migrations generated
- [ ] `docs/erd.md` updated with new entities — pending

## Completion Notes (2026-05-21)

Core retail schemas and endpoints are shipped: `WeighingScaleReading`, `LayawayPlan`, `LayawayPayment` schemas exist; `scale.go`, `layaway.go` handlers registered in router. Barcode lookup handler at `catalog_barcode.go` registered as `GET /catalog/barcode/{barcode}` (path differs from original spec's `?barcode=` query param). Serial/lot fields added to order lines. Remaining gaps: stock visibility endpoint, serial capture endpoint, pole display, RBAC permission seeding.

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
