# Sprint 12: Integrations & Webhooks — pos-api

**Status:** 🟡 Webhook Engine + Channel CRUD + ERP Stub + Hotel Events Complete — CRUD endpoints, delivery worker, HMAC signature, channel management HTTP layer, ERP sale_posted stub, hotel lifecycle NATS events, and webhook NATS dispatcher shipped; channel order ingestion, accounting export, multi-device sync, and eTIMS subscriber pending  
**Period:** December 2026 – January 2027  
**Last updated:** 2026-05-25  
**Audit note (2026-05-09):** eTIMS ownership corrected — treasury-api owns KRA submission; pos-api is a thin consumer of the result. FiscalReceipt entity removed from pos-api scope.  
**Goal:** External integrations, webhook delivery, channel sync (Uber Eats, Glovo, direct online ordering), accounting export, and multi-device synchronisation

---

## Context

The `ChannelIntegration`, `ChannelSyncJob`, `OrderLink`, `WebhookSubscription`, and `SyncFailure` schemas already exist. This sprint wires them into working HTTP handlers and background workers.

External integrations serve two purposes:
1. **Inbound**: receive orders from marketplace channels (online ordering, food delivery platforms) and create `PosOrder` entries that appear on the KDS
2. **Outbound**: push order events, inventory updates, and sales data to accounting, ERP, and third-party systems via webhooks

---

## Deliverables

### Webhook Engine
- [x] `POST /{tenant}/pos/webhooks` — register a webhook subscription (`webhooks.go` handler)
- [x] `GET /{tenant}/pos/webhooks` — list subscriptions
- [x] `PUT /{tenant}/pos/webhooks/{id}` — update subscription
- [x] `DELETE /{tenant}/pos/webhooks/{id}` — remove subscription
- [x] `GET /{tenant}/pos/webhooks/{id}/deliveries` — delivery log
- [x] `WebhookSubscription` schema (`internal/ent/schema/webhooksubscription.go`)
- [x] `WebhookDelivery` schema (`internal/ent/schema/webhookdelivery.go`)
- [x] Webhook delivery worker — `internal/modules/webhooks/delivery_worker.go` polls `webhook_deliveries` table every 10s, dispatches on event publish
- [ ] Delivery retry with exponential backoff — currently fixed 10s poll; exponential backoff not implemented
- [x] HMAC-SHA256 `X-Webhook-Signature` header — computed in `deliver()` when subscription has a secret

### Online Channel Order Ingestion
- [ ] `POST /{tenant}/pos/channels/{channel_id}/orders` — receive inbound order from marketplace (called by ordering-api or channel adapter)
- [ ] Maps external order format to `PosOrder` with `order_source = online`, `order_label = channel name`
- [ ] Creates KDS tickets automatically (same flow as in-restaurant orders)
- [ ] `ChannelSyncJob` record created for each inbound order; status tracked (received|processing|completed|failed)
- [ ] `OrderLink` record created linking `pos_order_id` ↔ `external_order_id`
- [x] `GET /{tenant}/pos/channels` — list configured channel integrations (`channels.go` handler)
- [x] `POST /{tenant}/pos/channels` — register a channel integration (name, type, credentials ref)
- [x] `PUT /{tenant}/pos/channels/{id}` — update channel config (note: PUT not PATCH)
- [x] `DELETE /{tenant}/pos/channels/{id}` — remove channel
- [x] `GET /{tenant}/pos/channels/{id}/sync-jobs` — list sync jobs for a channel
- [x] `POST /{tenant}/pos/channels/{id}/sync-jobs` — trigger a manual sync job

### Menu Push to Channels
- [ ] `POST /{tenant}/pos/channels/{channel_id}/sync-menu` — push current catalog to channel
- [ ] Translates `CatalogItem` to channel-specific format (Uber Eats, Glovo, generic webhook)
- [ ] Creates `ChannelSyncJob` record; async execution in background worker

### Accounting Export (Xero / QuickBooks / Sage)
- [ ] `IntegrationSetting` (existing schema) — used to store accounting platform credentials refs
- [ ] `POST /{tenant}/pos/integrations/accounting/sync` — push daily reconciliation to accounting platform
- [ ] Supports: generic journal entry CSV, Xero direct API (via XERO_CLIENT_ID/SECRET env), QuickBooks (future)
- [ ] `GET /{tenant}/pos/integrations/accounting/sync-log` — history of syncs

### ERP Integration (Codevertex ERP)
- [x] On `pos.sale.finalized`: publishes `erp.sale.posted` stub event to NATS with order summary — **pass-through until ERP integration is ready; no-op consumer on ERP side for now**
- [ ] ERP-api subscribes and creates sales invoices automatically — pending ERP-api implementation
- [ ] On `pos.drawer.closed`: publish cash position event to `erp.events` for ledger entry
- [ ] `GET /{tenant}/pos/integrations/erp/status` — last sync status + any failures

### Multi-Device Sync / Real-Time Cart Sharing
- [ ] `SharedCart` schema — `id, tenant_id, outlet_id, device_id, cart_data (JSON), updated_at` — enables multiple devices (e.g., waiter tablet + cashier terminal) to see the same cart
- [ ] `PUT /{tenant}/pos/carts/{cart_id}` — upsert shared cart state
- [ ] `GET /{tenant}/pos/carts/{cart_id}` — retrieve cart state
- [ ] Redis pub/sub for real-time cart updates (device subscribes to `pos:cart:{cart_id}`)

### Fiscal Device Integration (eTIMS / KRA) — pos-api Responsibilities Only

> **Architecture Decision (2026-05-09):** eTIMS fiscal submission is **owned by treasury-api**, not pos-api. treasury-api holds the KRA API credentials (`ETIMS_URL`, `ETIMS_CU_SERIAL`, `ETIMS_API_KEY`), owns the `FiscalReceipt` entity, and calls the KRA eTIMS API directly. pos-api's role is limited to:
>
> 1. Publishing `pos.sale.finalized` on order completion (already planned — Sprint 6 publisher).
> 2. Subscribing to `treasury.fiscal.signed` NATS event and writing `etims_invoice_number` + `etims_qr_code_url` to `pos_orders`.
> 3. Serving those fields in the `GET /{tenant}/pos/orders/{id}` response so pos-ui can render the receipt QR code.
>
> pos-api does **NOT** own a `FiscalReceipt` schema. Do **NOT** add `POST /{tenant}/pos/fiscal/sign` — that endpoint belongs in treasury-api. The `FISCAL_ETIMS_ENABLED` flag is also a treasury-api env var.

**pos-api Sprint 12 eTIMS tasks (revised):**
- [ ] Add nullable fields to `pos_orders` Ent schema: `etims_invoice_number (string, optional)`, `etims_qr_code_url (string, optional)`
- [ ] Run Atlas migration for new `pos_orders` fields
- [ ] Add NATS subscriber for `treasury.fiscal.signed` → write `etims_invoice_number` + `etims_qr_code_url` on the matching `pos_order`
- [ ] Add `pos.fiscal.view` permission (view fiscal status on order) — no `pos.fiscal.manage` needed; pos-api has no signing capability
- [ ] Ensure `GET /{tenant}/pos/orders/{id}` response includes `etims_invoice_number` and `etims_qr_code_url`

### RBAC Additions
- [ ] New permissions: `pos.webhooks.view`, `pos.webhooks.manage`
- [ ] New permissions: `pos.channels.view`, `pos.channels.manage`
- [ ] New permissions: `pos.integrations.view`, `pos.integrations.manage`
- [ ] New permissions: `pos.fiscal.view` (read eTIMS status on order — no signing capability in pos-api)

### Migration
- [ ] Add `SharedCart` ent schema
- [ ] Add `etims_invoice_number` + `etims_qr_code_url` nullable fields to `pos_orders` (NOT a separate FiscalReceipt entity)
- [ ] Run `go generate ./internal/ent`
- [ ] Generate Atlas migration: `integrations_module`
- [ ] Update `docs/erd.md`
- [ ] Update `docs/integrations.md` with channel adapter patterns

### Hotel Lifecycle Events (2026-05-25)

- [x] `hotel.guest.checked_in` — published on `POST /{tenant}/hotel/guests` (check-in); payload: guest_id, room_id, tenant_id
- [x] `hotel.guest.checked_out` — published on `POST /{tenant}/hotel/guests/{id}/checkout`; payload: guest_id, room_id, tenant_id
- [x] `hotel.folio.charge` — published when a POS order is closed with `room_charge` tender; payload: folio_item_id, room_guest_id, amount

### Webhook NATS Dispatcher (2026-05-25)

- [x] NATS subscriber (`internal/modules/webhooks/nats_dispatcher.go`) subscribes to `pos.>` wildcard subject
- [x] On each event, queries `WebhookSubscription` records matching the event key
- [x] Dispatches HTTP delivery to each matched subscription (creates `WebhookDelivery` record)
- [x] Works in conjunction with the existing delivery worker for retry handling

---

## Completion Notes (2026-05-25)

Webhook engine fully shipped (CRUD + delivery worker + HMAC). Channel management HTTP layer live (list, create, update, delete channels; list, trigger sync jobs). ERP `erp.sale.posted` stub event published on sale finalized — pass-through until ERP-api is ready. Hotel lifecycle NATS events (check-in, check-out, folio charge) published. Webhook NATS dispatcher wired to `pos.>` wildcard. Channel order ingestion (inbound marketplace orders), accounting export (Xero/QuickBooks), multi-device cart sync, and eTIMS subscriber remain pending.

---

## Use Cases Covered

| Use Case | Business Types |
|----------|---------------|
| Receive orders from Uber Eats / Glovo | Restaurant, fast food |
| Push menu changes to delivery platforms | Restaurant, fast food |
| Outbound webhooks for ERP/accounting | All business types |
| Xero / QuickBooks accounting sync | All business types |
| KRA eTIMS fiscal signing | All businesses (Kenyan tax compliance) |
| Multi-device cart sharing | Retail, restaurant, hotel |
| Real-time order events to third parties | All business types |
