# POS Service — Integration Guide

**Last updated:** 2026-05-21

> **Retail-POS revamp note (2026-06-07):** several ❌ statuses below are **stale**. Verified against
> code 2026-06-07: pos **loyalty auto-earn on `pos.sale.finalized` IS wired** (`SaleFinalizedSubscriber`),
> inventory **publishes `inventory.purchase_order.received`**, and treasury **AR/AP aging endpoints
> are live** (`/ar/aging`, `/ap/aging`). **Loyalty is now the pos-api source of truth** (keyed on
> `crm_contact_id`; ordering-backend is a client). New retail surfaces (pricing profile, parked
> sales, return-by-invoice, credit sale→AR, cheque/bank/advance/store-credit tenders, M-Pesa C2B
> cashier reconciliation, add-expense-from-register) are scoped in the gap matrix. Authoritative
> current state + roadmap: `/.claude/plans/_audit-parts/retail-pos-gap-matrix.md` and
> `/.claude/plans/_audit-parts/retail-pos-audit-and-roadmap-2026-06-07.md`. Re-verify any specific ❌ line against
> code before relying on it.

## Overview

The POS service is the **source of truth for sales catalogs (menus)**. While `inventory-api` owns the physical item master, `pos-api` owns how those items are grouped, priced, and displayed for sale at an outlet.

---

## eTIMS Ownership — Architecture Decision Record

**Decision (2026-05-09):** KRA eTIMS fiscal submission is owned by **treasury-api**, not pos-api.

**Rationale:**
- treasury-api already owns all invoicing, tax calculation, and payment settlement for the Codevertex platform.
- eTIMS is a tax-invoice signing and transmission obligation — it belongs alongside the invoicing ledger, not the POS transaction recorder.
- pos-api is a **thin client** for non-cash payments: it creates a payment intent in treasury-api, then reacts to the result. eTIMS follows the same pattern — pos-api passes invoice data to treasury-api, which handles signing, KRA transmission, and QR code generation.
- This avoids duplicating KRA API credentials and the OSCU/VSCU device serial across two services.

**Correct flow for eTIMS:**
```
1. pos-api completes an order → publishes pos.sale.finalized (outbox)
2. treasury-api consumes pos.sale.finalized
3. treasury-api submits invoice to KRA eTIMS API (OSCU/VSCU mode)
4. treasury-api stores record in etims_invoices table, publishes treasury.etims.invoice_transmitted
5. pos-api consumes treasury.etims.invoice_transmitted → stores etims_invoice_number + qr_code_url on pos_order
6. pos-ui receipt renders eTIMS QR code from the pos_order response (not directly from treasury-api)
```

**What pos-api does NOT do:**
- pos-api does NOT call the KRA eTIMS API directly.
- pos-api does NOT store the raw KRA API credentials (ETIMS_URL, ETIMS_CU_SERIAL, ETIMS_API_KEY).
- pos-api does NOT own an EtimsInvoice entity — treasury-api owns that (table: `etims_invoices`).
- pos-api does NOT own quotations or sales credit-notes either — treasury owns ALL financial documents
  (invoices, quotations, proforma, sales credit-notes). A pos sales return that needs a VAT-reversal
  credit note calls treasury S2S `POST /api/v1/s2s/{tenant}/invoices/{invoiceID}/create-credit-note`
  (reuses `invoicing.CreateCreditNote`); a pos "Save as Quotation" calls a treasury quotation S2S create.
  A standalone pos `CreditNote` entity was built once and DISCARDED — do not reintroduce it. pos-ui LINKS
  to treasury-ui for these documents (never duplicates the pages); only the create-from-pos action lives here.

**What pos-api does store:**
- `pos_orders.etims_invoice_number` (nullable string) — populated after `treasury.etims.invoice_transmitted` is received.
- `pos_orders.etims_qr_code_url` (nullable string) — populated from `treasury.etims.invoice_transmitted` payload.

**Environment variables (treasury-api, NOT pos-api):**
- `ETIMS_URL`, `ETIMS_CU_SERIAL`, `ETIMS_API_KEY` belong in treasury-api only.

**Sprint 12 (pos-api) correction:** Sprint 12 originally listed a `FiscalReceipt` schema and `POST /{tenant}/pos/fiscal/sign` endpoint inside pos-api. This is incorrect. Sprint 12 work in pos-api is limited to:
- Adding `etims_invoice_number` and `etims_qr_code_url` nullable fields to `pos_orders`.
- Adding the NATS subscriber for `treasury.etims.invoice_transmitted` to populate those fields.
- Ensuring receipt PDFs and pos-ui display the QR code from `pos_order` data.

See also: [Sprint 12](sprints/sprint-12-integrations-webhooks.md) and [Sprint 5](sprints/sprint-5-erp-gaps.md).

---

## 1. Inventory Service Integration

### 1.1 Catalog Sync (Background Worker)

**Trigger:** NATS `inventory.catalog.updated` event  
**Subscriber:** `internal/platform/events/subscribers.go`  
**Action:** Fetch updated items from `GET /v1/{tenant}/inventory/items`, upsert `catalog_items` projection

**Status:** ❌ NATS subscriber not yet wired (Sprint 6 — remaining gap)

### 1.2 Stock Consumption Backflush

**Trigger:** `pos.sale.finalized` outbox event (published on order completion)  
**Action:** pos-api calls `POST /v1/{tenant}/inventory/consumption`

```
POST https://inventoryapi.codevertexitsolutions.com/v1/{tenant}/inventory/consumption
X-API-Key: {INTERNAL_SERVICE_KEY}
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
**Env vars:** `INVENTORY_SERVICE_URL`, `INTERNAL_SERVICE_KEY`  
**Retry:** 3 attempts with exponential backoff  
**Status:** ❌ HTTP call not yet wired in `orders.Service.Complete()` (Sprint 6 — remaining gap)

### 1.2b Transaction Reversal (platform-owner data-repair tool, 2026-07-17)

**Trigger:** Platform owner runs a reversal from pos-ui Sync Monitor → "Txn Reversals" tab
(`POST /api/v1/{tenant}/pos/reversals`, platform-owner JWT only).
**What it does:** Reverses a FINALIZED sale (whole order or selected items) across every
integrated service, each step tracked on the `pos_reversals` row (steps JSON) and retryable
(`POST /reversals/{id}/retry`):

1. `pos_totals` — soft-voids the lines; partial: `RecomputeTotals` + payments/paid_total
   netted down; full: order → `refunded`, totals/payments kept + stamped.
2. `inventory_consumption` — `POST /v1/{tenant}/inventory/consumption/reverse` (S2S):
   BOM-accurate add-back net of recorded shortfall, negative compensating consumption
   lines/dailies, idempotent on `pos-reversal-{id}`.
3. `treasury_gl` — reuses the returns settlement `POST /api/v1/s2s/{tenant}/refunds`
   (revenue+VAT reversal, catalog-source COGS reversal, AR netting, auto credit-note doc);
   idempotent on the reversal id.
4. `etims_credit_note` — `GetInvoiceByReference("pos_order", orderID)` + `CreateCreditNote`
   when the sale has a treasury tax invoice; SKIPPED otherwise.

**Modules:** `internal/modules/reversals` (orchestrator), `handlers/reversals.go`,
`ent/schema/posreversal.go`. Distinct from POSReturn (customer returns lifecycle).

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
       X-API-Key: {INTERNAL_SERVICE_KEY}
       {
         "source_service": "pos",
         "reference_id": "<order_id>",
         "reference_type": "pos_order",
         "amount": 1500,
         "currency": "KES",
         "payment_method": "paystack"|"mpesa",
         "customer_id": "<customer_uuid>"
       }
     ← 201 Created (M-Pesa):  { "intent_id": "...", "checkout_request_id": "..." }
     ← 201 Created (Paystack): { "intent_id": "...", "authorization_url": "..." }
     → store intent_id in pos_payments.external_reference
     → return { status: "pending", intent_id, checkout_url|mpesa_request_id } to pos-ui

3. pos-ui:
   M-Pesa: show STK push waiting screen, poll GET /{tenant}/pos/orders/{id}/payments every 3s
   Card: redirect to authorization_url (Paystack checkout)

4. treasury.payment.success NATS event
   → pos-api marks pos_payments.status = "completed"
   → order auto-completed if fully paid

5. treasury.payment.failed NATS event
   → pos-api marks pos_payments.status = "failed"
   → notify pos-ui
```

**Client:** `internal/modules/treasury/client.go`  
**Env vars:** `TREASURY_SERVICE_URL=https://booksapi.codevertexitsolutions.com`, `INTERNAL_SERVICE_KEY`  
**Auth:** `X-API-Key: {INTERNAL_SERVICE_KEY}` header (S2S) — same shared key for all services  
**Status:** ❌ S2S intent creation not yet wired in `payments.Service.Record()` (Sprint 6)

> **Schema note (2026-05-09):** The actual `pos_payments` Ent schema uses field `status` (not `payment_status`) and `external_reference` (not `provider_reference`). Store the treasury `intent_id` in `pos_payments.external_reference`. Valid `pos_payments.status` values: `pending`, `completed`, `failed`, `refunded`.

> **Publisher note (2026-05-09):** The current `internal/platform/events/publisher.go` only defines three methods: `PublishOrderCreated`, `PublishOrderStatusChanged`, `PublishPaymentRecorded`. The `pos.sale.finalized` and `pos.drawer.closed` events described in the Event Catalog are **planned but not yet published** — they must be added to `publisher.go` in Sprint 6.

### 2.2 Room Charge Settlement (Hotel Module)

On hotel check-out, pos-api creates a single treasury payment intent for the full folio amount.

```
POST /api/v1/s2s/{tenant}/payments/intents
X-API-Key: {INTERNAL_SERVICE_KEY}

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

**Status:** ✅ Event published (outbox). ❌ Treasury NATS subscriber not yet wired (treasury-api responsibility — Sprint 3)

### 2.4 NATS Events from Treasury

| Event | Action in pos-api |
|-------|-------------------|
| `treasury.payment.success` | Mark `pos_payments.status = completed`, auto-complete order |
| `treasury.payment.failed` | Mark `pos_payments.status = failed`, notify pos-ui |
| `treasury.etims.invoice_transmitted` | Populate `pos_orders.etims_invoice_number` + `etims_qr_code_url` for receipt printing |

**Status:** ❌ NATS subscribers not yet wired (Sprint 6 for payment events; Sprint 12 for eTIMS event)

---

## 3. Ordering Backend Integration

### 3.1 Catalog Sync (outbound)

pos-api publishes `pos.menu.updated` on any `CatalogItem` or `CatalogCategory` change.  
ordering-backend subscribes and updates its storefront projection.

**Status:** ✅ Event published on catalog write operations.

### 3.2 Online Order → KDS Ticket Creation (CRITICAL GAP)

**Background:** Hospitality businesses (restaurant, bar, hotel dining) receive online orders via ordering-backend. When a dine-in or pickup order reaches `confirmed` or `preparing` status, the kitchen must see a KDS ticket in pos-api. Currently, this link does not exist.

**Current state (ordering-backend side):**
- On order status change → ordering-backend publishes `ordering.order.status.changed` to NATS JetStream
- For `ready` status, also publishes `ordering.order.ready` (logistics) and `ordering.order.for_pickup` (POS pickup handoff)
- **No KDS ticket creation anywhere in the ordering-backend codebase**

**Required integration (pos-api side — Sprint 13):**
- pos-api subscribes to `ordering.order.status.changed`
- Filters for: `new_status IN (confirmed, preparing)` AND `fulfillment_type IN (dine_in, pickup)`
- Creates `KDSTicket` entries per line item, routed to station by item category (`kitchen`, `bar`, `grill`)
- Marks order lines `kds_status = sent`

**Completion callback:**
- When kitchen marks KDS ticket complete (`kds_status = ready`), pos-api publishes `pos.kds.ticket.ready`
- ordering-backend may optionally subscribe to update order status to `ready` for same-table orders

**NATS Subject:** `ordering.order.status.changed`  
**Filter fields:** `new_status`, `fulfillment_type`, `tenant_id`, `outlet_id`  
**Status:** ❌ Not implemented — see [Sprint 13](sprints/sprint-13-ordering-kds-integration.md)

### 3.3 Pickup Order Handoff (existing)

For `fulfillment_type = pickup`, ordering-backend publishes `ordering.order.for_pickup`. pos-api creates a POS order for cashier settlement.

**Status:** ✅ Event consumed. Pickup orders appear in pos-api with `order_source = online`.

---

## 4. Auth Service Integration

### 4.1 JWT Validation (SSO Login)

All pos-api protected routes under `/{tenant}/pos/` validate Bearer tokens issued by auth-api (RS256, audience `codevertex`).

**Library:** `shared/auth-client` v0.1.0  
**Env vars:** `AUTH_SERVICE_URL`, `AUTH_AUDIENCE=codevertex`  
**Status:** ✅ Implemented

**Flow:**
1. pos-ui redirects to auth-api OAuth2 PKCE endpoint (`/oauth2/authorize`)
2. User logs in via SSO (Google, Microsoft) or email/password
3. auth-api issues access token (15 min) + refresh token (30 days)
4. pos-ui stores tokens in localStorage, sends `Authorization: Bearer {token}` on all API calls

**Suitable for:** Manager, admin, and office-based staff who have SSO accounts

### 4.2 Terminal PIN Login (CRITICAL GAP)

**Background:** The hotel-pos-v8.jsx design requires a touchscreen PIN login (4–6 digits) for kitchen staff, waiters, cashiers, bar staff, and receptionists. These users cannot go through a browser OAuth2 redirect on a dedicated POS terminal.

**Current state (2026-05-21):** ✅ IMPLEMENTED. PIN login is fully operational.

**Implemented:**
- `pin_hash`, `pin_failed_attempts`, `pin_locked_until` fields on `staff_members` Ent schema
- `POST /{tenant}/pos/auth/pin` — validates PIN, issues 4-hour terminal JWT (HMAC-SHA256)
- `POST /{tenant}/pos/auth/pin/set` — manager sets/resets staff PIN (requires `pos.staff.manage`)
- `GET /{tenant}/pos/staff` — list staff for PIN selector grid (public route)
- `GET /{tenant}/pos/auth/pin/profile` — staff profile for offline PIN auth
- Terminal JWT embeds: `outlet_id`, `outlet_code`, `outlet_use_case`, `is_hq_user`
- pos-ui: PIN kiosk at `/[orgSlug]/pin-login` with avatar grid + 12-button keypad
- Offline PIN validation via bcrypt against IndexedDB cached `staffProfiles`
- `GET /{tenant}/pos/auth/me` — Trinity Layer 3: returns POS role + permissions for SSO users

**Token type:** `pos_terminal` HMAC-SHA256 JWT, 4-hour expiry, stored in `sessionStorage`  
**Status:** ✅ Complete — see [pos-ui Sprint 10](../../pos-ui/docs/sprints/sprint-10-pos-auth.md)

---

## 5. Webhook Delivery System (Sprint 12)

### Webhook Subscription CRUD

Registered in router under `/{tenantID}/pos/webhooks/`:

| Method | Path | Description |
|--------|------|-------------|
| GET | `/webhooks` | List subscriptions |
| POST | `/webhooks` | Create subscription (URL, events, secret) |
| PUT | `/webhooks/{id}` | Update subscription |
| DELETE | `/webhooks/{id}` | Remove subscription |
| GET | `/webhooks/{id}/deliveries` | Delivery log |

**Schemas:** `WebhookSubscription` (`webhooksubscription.go`), `WebhookDelivery` (`webhookdelivery.go`)  
**Status:** ✅ CRUD implemented. ❌ Delivery worker (NATS fan-out, retry, HMAC signing) not yet implemented.

---

## 6. Online Order Pickup (Sprint 13)

### Click-and-Collect Pickup Endpoints

Registered in router under `/{tenantID}/pos/online-orders/`:

| Method | Path | Description |
|--------|------|-------------|
| GET | `/online-orders/pickup` | List orders awaiting pickup |
| POST | `/online-orders/{id}/ready` | Mark order ready for collection |
| POST | `/online-orders/{id}/collected` | Mark order collected |

**Handler:** `internal/http/handlers/online_orders.go`  
**Status:** ✅ Pickup endpoints implemented. ❌ NATS subscriber for `ordering.order.status.changed` → KDS ticket creation not yet implemented (see Sprint 13).

---

## 7. Notifications Service Integration

**Used for:**
- KDS waiter-call notifications (`pos.kds.waiter.called` → notifications-service push)
- KDS ticket ready notifications (`pos.kds.ticket.ready`)
- Hotel check-in/check-out confirmations
- Stock alert notifications

**Client:** `internal/modules/notifications/client.go` (planned)  
**Env vars:** `NOTIFICATIONS_SERVICE_URL`, `INTERNAL_SERVICE_KEY`

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
| `treasury.payment.success` | treasury-api | Mark payment succeeded (`pos_payments.status = completed`), complete order | ❌ Not wired — Sprint 6 |
| `treasury.payment.failed` | treasury-api | Mark payment failed (`pos_payments.status = failed`) | ❌ Not wired — Sprint 6 |
| `treasury.etims.invoice_transmitted` | treasury-api | Populate `pos_orders.etims_invoice_number` + `etims_qr_code_url` for receipt display | ❌ Not wired — Sprint 12 |
| `ordering.order.status.changed` | ordering-backend | Create KDS ticket when hospitality order reaches `confirmed`/`preparing` | ❌ Not wired — Sprint 13 |

---

## Environment Variables

```bash
# Single S2S key used for ALL outbound service calls (X-API-Key header)
INTERNAL_SERVICE_KEY=<platform shared S2S key>

# Service URLs
INVENTORY_SERVICE_URL=https://inventoryapi.codevertexitsolutions.com
TREASURY_SERVICE_URL=https://booksapi.codevertexitsolutions.com
NOTIFICATIONS_SERVICE_URL=https://notificationsapi.codevertexitsolutions.com
ORDERING_SERVICE_URL=https://orderingapi.codevertexitsolutions.com
```

**S2S Auth Standard**: All Codevertex internal services use a single `INTERNAL_SERVICE_KEY` env var. The same key value is sent as `X-API-Key` header on every S2S call regardless of the target service. Each receiving service validates it against its own `INTERNAL_SERVICE_KEY`. There are no per-service API keys.
