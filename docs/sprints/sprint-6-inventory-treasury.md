# Sprint 6: Inventory & Treasury Integration — pos-api

**Status:** 🟡 Partially Complete — S2S client files exist; NATS subscribers not yet wired; env vars not in devops  
**Period:** June–July 2026  
**Last updated:** 2026-05-21  
**Goal:** Wire pos-api → inventory-api stock consumption, wire pos-api → treasury-api payment intent workflow for card/M-Pesa, wire NATS subscribers

> **eTIMS ownership confirmed**: treasury-api owns all KRA eTIMS transmission. This sprint does NOT include any eTIMS work in pos-api. eTIMS subscriber work (`treasury.etims.invoice_transmitted`) is Sprint 12 only.

> **S2S auth standard**: Use `INTERNAL_SERVICE_KEY` as the single `X-API-Key` header for ALL outbound service calls (treasury-api, inventory-api, notifications-api). Do NOT create per-service key env vars.

> **Schema note**: Store treasury `intent_id` in `pos_payments.external_reference` (not `provider_reference`). The field on the Ent schema is `external_reference`. The payment status field is `pos_payments.status` — valid values: `pending`, `completed`, `failed`, `refunded`.

---

## Context

pos-api already defines the data model for inventory touchpoints and publishes outbox events. The actual HTTP calls to inventory-api and treasury-api are not yet wired.

---

## Part A: Inventory Integration

### A1: Stock Consumption Backflush
**Trigger:** `pos.sale.finalized` outbox event published on order completion  
**Action:** pos-api calls `POST /v1/{tenant}/inventory/consumption`

```go
// internal/modules/inventory/client.go
type InventoryClient interface {
    PostConsumption(ctx context.Context, tenantSlug string, req ConsumptionRequest) error
}

type ConsumptionRequest struct {
    POSOrderID string            `json:"pos_order_id"`
    Items      []ConsumptionItem `json:"items"`
    OutletID   string            `json:"outlet_id"`
}
type ConsumptionItem struct {
    SKU      string  `json:"sku"`
    Quantity float64 `json:"quantity"`
    UOMCode  string  `json:"uom_code"`
}
```

Implementation using `shared-service-client` with:
- Env: `INVENTORY_SERVICE_URL`, `INTERNAL_SERVICE_KEY`
- Auth: `X-API-Key: {INTERNAL_SERVICE_KEY}` header (S2S) — same key for all services
- Retry: 3 attempts with exponential backoff (via shared-service-client)

### A2: Catalog Sync (Background Worker)
**Trigger:** NATS `inventory.catalog.updated` event  
**Action:** Fetch updated items, upsert `catalog_items` projection

```go
// internal/platform/events/subscribers.go — add subscriber:
nc.Subscribe("inventory.events", func(msg *nats.Msg) {
    // Filter for catalog.updated events
    // Call inventory-api GET /v1/{tenant}/inventory/items
    // Upsert catalog_items table
})
```

### A3: Stock Alert Subscriber
**Trigger:** NATS `inventory.stock.low` event  
**Action:** Create notification via notifications-service (or mark alert in `stock_alert_subscriptions`)

---

## Part B: Treasury Integration

### B1: Payment Intent Workflow (Card/M-Pesa)

**Flow:**
```
pos-ui → POST /{tenant}/pos/orders/{id}/payments {tender_type: "card"|"mpesa", amount}
  → pos-api payments handler
    → if cash: record immediately, auto-complete order
    → if card/mpesa:
        call treasury-api POST https://booksapi.codevertexitsolutions.com/api/v1/s2s/{tenant}/payments/intents
        X-API-Key: {INTERNAL_SERVICE_KEY}
        {source_service: "pos", reference_id: order_id, reference_type: "pos_order",
         amount, currency: "KES", payment_method: "paystack"|"mpesa", customer_id}
        ← 201 (M-Pesa):   {intent_id, checkout_request_id}
        ← 201 (Paystack): {intent_id, authorization_url}
        → store intent_id in pos_payments.external_reference   (NOT provider_reference)
        → return {status: "pending", intent_id, checkout_url|mpesa_request_id} to pos-ui
  → pos-ui polls GET /{tenant}/pos/orders/{id}/payments for status update
  → treasury.payment.success NATS event → pos-api marks pos_payments.status = "completed"
  → order auto-completed
```

**Environment variables needed:**
- `TREASURY_SERVICE_URL` — e.g., `https://booksapi.codevertexitsolutions.com`
- `INTERNAL_SERVICE_KEY` — shared platform S2S key (same for all services)

**Treasury client:**
```go
// internal/modules/treasury/client.go
type TreasuryClient interface {
    CreatePaymentIntent(ctx context.Context, tenantSlug string, req PaymentIntentRequest) (*PaymentIntentResponse, error)
}
```

### B2: Room Charge Settlement on Check-out
- On room check-out: calculate total folio amount
- Create single treasury intent for full stay amount
- Payment method determined at check-out (cash/card/mpesa)
- On payment success: mark `RoomGuest.status = checked_out`

### B3: Cash Drawer Close → Treasury Ledger
**Trigger:** Cash drawer close event  
**Action:** Publish `pos.drawer.closed` with cash position → treasury-api creates ledger entry

---

## Part C: NATS Subscribers to Wire

| Event | Publisher | Current Status | Action |
|-------|-----------|----------------|--------|
| `inventory.catalog.updated` | inventory-api | ❌ Not subscribed | Refresh catalog_items |
| `inventory.stock.low` | inventory-api | ❌ Not subscribed | Alert notification |
| `treasury.payment.success` | treasury-api | ❌ Not subscribed | Mark payment succeeded, complete order |
| `treasury.payment.failed` | treasury-api | ❌ Not subscribed | Mark payment failed |

---

## Environment Variables to Add
```bash
INTERNAL_SERVICE_KEY=<platform shared S2S key>
INVENTORY_SERVICE_URL=https://inventoryapi.codevertexitsolutions.com
TREASURY_SERVICE_URL=https://booksapi.codevertexitsolutions.com
NOTIFICATIONS_SERVICE_URL=https://notificationsapi.codevertexitsolutions.com
ORDERING_SERVICE_URL=https://orderingapi.codevertexitsolutions.com
```

**S2S Auth Standard**: All BengoBox services use a single `INTERNAL_SERVICE_KEY` env var. The same key value is sent as `X-API-Key` header to every internal service. Do not create per-service key env vars (e.g., no `TREASURY_API_KEY` or `INVENTORY_API_KEY` — they all use `INTERNAL_SERVICE_KEY`).

---

## Tasks
- [~] Create `internal/modules/inventory/client.go` (S2S inventory client) — file exists per architecture docs; HTTP call not confirmed wired in orders.Service.Complete()
- [ ] Wire consumption call in `orders.Service.Complete()` — NOT wired; integrations.md explicitly states this is missing
- [~] Create `internal/modules/treasury/client.go` (S2S treasury client) — file referenced in Sprint 2 deliverables; intent endpoint registered in router but S2S call to treasury-api not confirmed wired
- [ ] Wire treasury intent creation in `payments.Service.Record()` for non-cash tenders — integrations.md states "❌ S2S intent creation not yet wired"
- [ ] Add NATS subscriber for `treasury.payment.success` / `treasury.payment.failed` — integrations.md states "❌ Not wired — Sprint 6"
- [ ] Add NATS subscriber for `inventory.catalog.updated` — integrations.md states "❌ Not wired"
- [ ] Add NATS subscriber for `inventory.stock.low` — integrations.md states "❌ Not wired"
- [ ] Add env vars to devops-k8s `apps/pos-service/values.yaml`
- [ ] Update `docs/integrations.md` with complete treasury + inventory flows
- [x] Build and fix all errors: `go build ./...`
- [x] Push to staging, merge to main

## Status as of 2026-05-21

Integration S2S client files referenced in Sprint 2 notes and architecture docs but NATS subscribers are explicitly documented as NOT wired in `docs/integrations.md` and `docs/architecture.md`. The payment intent endpoint (`POST /orders/{id}/payments/intent`) is registered in the router and handler exists, but the actual HTTP call to treasury-api from inside `payments.Service` is unconfirmed. All NATS event subscriptions remain unwired. Sprint 6 is the primary blocker for M-Pesa/card payment completion and inventory backflush.
