# Sprint 6: Inventory & Treasury Integration — pos-api

**Status:** 🟡 Partial (events defined, wiring incomplete)  
**Period:** June–July 2026  
**Goal:** Wire pos-api → inventory-api stock consumption, wire pos-api → treasury-api payment intent workflow for card/M-Pesa, wire NATS subscribers

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
- Env: `INVENTORY_SERVICE_URL`, `INVENTORY_SERVICE_API_KEY`
- Auth: `X-API-Key: {api_key}` header (S2S)
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
        call treasury-api POST /api/v1/s2s/{tenant}/payments/intents
        {source_service: "pos", reference_id: order_id, reference_type: "pos_order",
         amount, currency, payment_method: "paystack"|"mpesa", customer_id}
        → returns {intent_id, checkout_request_id|authorization_url}
        → store intent_id in pos_payments.provider_reference
        → return {status: "pending", intent_id, checkout_url|mpesa_request_id} to pos-ui
  → pos-ui polls GET /{tenant}/pos/orders/{id}/payments for status update
  → treasury.payment.success NATS event → pos-api marks payment succeeded
  → order auto-completed
```

**Environment variables needed:**
- `TREASURY_SERVICE_URL` — e.g., `https://booksapi.codevertexitsolutions.com`
- `TREASURY_SERVICE_API_KEY` — S2S API key from auth-api

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
INVENTORY_SERVICE_URL=https://inventoryapi.codevertexitsolutions.com
INVENTORY_SERVICE_API_KEY=<from auth-api>
TREASURY_SERVICE_URL=https://booksapi.codevertexitsolutions.com
TREASURY_SERVICE_API_KEY=<from auth-api>
NOTIFICATIONS_SERVICE_URL=https://notificationsapi.codevertexitsolutions.com
NOTIFICATIONS_SERVICE_API_KEY=<from auth-api>
```

---

## Tasks
- [ ] Create `internal/modules/inventory/client.go` (S2S inventory client)
- [ ] Wire consumption call in `orders.Service.Complete()`
- [ ] Create `internal/modules/treasury/client.go` (S2S treasury client)
- [ ] Wire treasury intent creation in `payments.Service.Record()` for non-cash tenders
- [ ] Add NATS subscriber for `treasury.payment.success` / `treasury.payment.failed`
- [ ] Add NATS subscriber for `inventory.catalog.updated`
- [ ] Add NATS subscriber for `inventory.stock.low`
- [ ] Add env vars to devops-k8s `apps/pos-service/values.yaml`
- [ ] Update `docs/integrations.md` with complete treasury + inventory flows
- [ ] Build and fix all errors: `go build ./...`
- [ ] Push to staging, merge to main
