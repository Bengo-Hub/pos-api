# POS Service — Integration Guide

**Last updated:** 2026-05-07

## Overview

The POS service is the **source of truth for sales catalogs (menus)**. While `inventory-api` owns the physical item master, `pos-api` owns how those items are grouped, priced, and displayed for sale at an outlet.

---

## 1. Inventory Service Integration

### 1.1 Catalog Sync (Background Worker)

**Trigger:** NATS `inventory.catalog.updated` event  
**Subscriber:** `internal/platform/events/subscribers.go`  
**Action:** Fetch updated items from `GET /v1/{tenant}/inventory/items`, upsert `catalog_items` projection

**Status:** ❌ NATS subscriber not yet wired (Sprint 6)

### 1.2 Stock Consumption Backflush

**Trigger:** `pos.sale.finalized` outbox event (published on order completion)  
**Action:** pos-api calls `POST /v1/{tenant}/inventory/consumption`

```
POST https://inventoryapi.codevertexitsolutions.com/v1/{tenant}/inventory/consumption
X-API-Key: {INVENTORY_SERVICE_API_KEY}
Content-Type: application/json

{
  "pos_order_id": "uuid",
  "outlet_id": "uuid",
  "items": [
    { "sku": "ITEM-001", "quantity": 2.0, "uom_code": "EA" }
  ]
}
```

**Client:** `internal/modules/inventory/client.go` (S2S via `shared-service-client`)  
**Env vars:** `INVENTORY_SERVICE_URL`, `INVENTORY_SERVICE_API_KEY`  
**Retry:** 3 attempts with exponential backoff  
**Status:** ❌ HTTP call not yet wired in `orders.Service.Complete()` (Sprint 6)

### 1.3 Stock Alert Subscriber

**Trigger:** NATS `inventory.stock.low` event  
**Action:** Create notification entry in `stock_alert_subscriptions` or call notifications-service  
**Status:** ❌ NATS subscriber not yet wired (Sprint 6)

---

## 2. Treasury Service Integration

### 2.1 Payment Intent Workflow (Card / M-Pesa)

For non-cash tenders, pos-api creates a payment intent in treasury-api before recording the payment.

**Full flow:**
```
1. pos-ui → POST /{tenant}/pos/orders/{id}/payments
            { tender_type: "card"|"mpesa", amount: 1500, currency: "KES" }

2. pos-api payments handler:
   if cash → record immediately, auto-complete order
   if card/mpesa:
     → POST https://booksapi.codevertexitsolutions.com/api/v1/s2s/{tenant}/payments/intents
       {
         "source_service": "pos",
         "reference_id": "<order_id>",
         "reference_type": "pos_order",
         "amount": 1500,
         "currency": "KES",
         "payment_method": "paystack"|"mpesa",
         "customer_id": "<customer_uuid>"
       }
     ← { "intent_id": "...", "checkout_request_id": "..." }   (M-Pesa)
     ← { "intent_id": "...", "authorization_url": "..." }     (Card/Paystack)
     → store intent_id in pos_payments.provider_reference
     → return { status: "pending", intent_id, checkout_url|mpesa_request_id } to pos-ui

3. pos-ui:
   M-Pesa: show STK push waiting screen, poll GET /{tenant}/pos/orders/{id}/payments every 3s
   Card: redirect to authorization_url (Paystack checkout)

4. treasury.payment.success NATS event
   → pos-api marks pos_payments.payment_status = "succeeded"
   → order auto-completed if fully paid

5. treasury.payment.failed NATS event
   → pos-api marks pos_payments.payment_status = "failed"
   → notify pos-ui
```

**Client:** `internal/modules/treasury/client.go`  
**Env vars:** `TREASURY_SERVICE_URL=https://booksapi.codevertexitsolutions.com`, `TREASURY_SERVICE_API_KEY`  
**Auth:** `X-API-Key: {TREASURY_SERVICE_API_KEY}` header (S2S)  
**Status:** ❌ S2S intent creation not yet wired in `payments.Service.Record()` (Sprint 6)

### 2.2 Room Charge Settlement (Hotel Module)

On hotel check-out, pos-api creates a single treasury payment intent for the full folio amount.

```
POST /api/v1/s2s/{tenant}/payments/intents
{
  "source_service": "pos",
  "reference_id": "<room_guest_id>",
  "reference_type": "room_folio",
  "amount": <total_folio_amount>,
  "currency": "KES",
  "payment_method": "cash"|"paystack"|"mpesa"
}
```

**Status:** ❌ Not yet implemented (requires hotel module — Sprint 3 + Sprint 6)

### 2.3 Cash Drawer Close → Treasury Ledger

**Trigger:** Cash drawer close  
**Action:** pos-api publishes `pos.drawer.closed` outbox event  
**treasury-api** subscribes and creates a ledger entry for the cash position

**Status:** ✅ Event published. ❌ Treasury NATS subscriber (treasury-api side — not pos-api responsibility)

### 2.4 NATS Events from Treasury

| Event | Action in pos-api |
|-------|-------------------|
| `treasury.payment.success` | Mark `pos_payments.payment_status = succeeded`, auto-complete order |
| `treasury.payment.failed` | Mark `pos_payments.payment_status = failed` |

**Status:** ❌ NATS subscribers not yet wired (Sprint 6)

---

## 3. Ordering Backend Integration

**Catalog Sync:** pos-api publishes `pos.menu.updated` events → ordering-backend consumes to update online storefront projection  
**Order Handoff:** Online-for-pickup orders initiated in ordering-backend are handed off to pos-api for fulfillment and KDS routing

---

## 4. Notifications Service Integration

**Used for:**
- KDS waiter-call notifications (`pos.kds.waiter.called` → notifications-service push)
- KDS ticket ready notifications (`pos.kds.ticket.ready`)
- Hotel check-in/check-out confirmations
- Stock alert notifications

**Client:** `internal/modules/notifications/client.go` (planned)  
**Env vars:** `NOTIFICATIONS_SERVICE_URL`, `NOTIFICATIONS_SERVICE_API_KEY`

---

## Event Catalog

### Events Published (Outbox)

| Event | Trigger | Consumers |
|-------|---------|-----------|
| `pos.sale.finalized` | Order completed | inventory-api (backflush), treasury-api (ledger) |
| `pos.drawer.closed` | Cash drawer closed | treasury-api (ledger entry) |
| `pos.menu.updated` | Catalog item changed | ordering-backend (storefront sync) |
| `pos.kds.ticket.ready` | KDS ticket marked ready | notifications-service (waiter push) |
| `pos.kds.waiter.called` | Call waiter triggered | notifications-service (waiter push) |
| `pos.room.checked_in` | Hotel check-in | notifications-service |
| `pos.room.checked_out` | Hotel check-out | notifications-service, treasury-api |
| `pos.return.completed` | Return approved | inventory-api (restock), treasury-api (refund) |
| `pos.daily_closing.completed` | Daily close run | treasury-api (reconciliation) |

### Events Consumed (NATS Subscribers)

| Event | Publisher | Action | Status |
|-------|-----------|--------|--------|
| `inventory.catalog.updated` | inventory-api | Refresh `catalog_items` projection | ❌ Not wired |
| `inventory.stock.low` | inventory-api | Create stock alert notification | ❌ Not wired |
| `treasury.payment.success` | treasury-api | Mark payment succeeded, complete order | ❌ Not wired |
| `treasury.payment.failed` | treasury-api | Mark payment failed | ❌ Not wired |

---

## Environment Variables

```bash
# Inventory
INVENTORY_SERVICE_URL=https://inventoryapi.codevertexitsolutions.com
INVENTORY_SERVICE_API_KEY=<from auth-api S2S>

# Treasury
TREASURY_SERVICE_URL=https://booksapi.codevertexitsolutions.com
TREASURY_SERVICE_API_KEY=<from auth-api S2S>

# Notifications
NOTIFICATIONS_SERVICE_URL=https://notificationsapi.codevertexitsolutions.com
NOTIFICATIONS_SERVICE_API_KEY=<from auth-api S2S>
```
