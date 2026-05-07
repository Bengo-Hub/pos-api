# Sprint 12: Integrations & Webhooks — pos-api

**Status:** 🔴 Not Started  
**Period:** December 2026 – January 2027  
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
- [ ] `POST /{tenant}/pos/webhooks` — register a webhook subscription (URL, events, secret)
- [ ] `GET /{tenant}/pos/webhooks` — list subscriptions
- [ ] `PATCH /{tenant}/pos/webhooks/{id}` — update (URL, events, active)
- [ ] `DELETE /{tenant}/pos/webhooks/{id}` — remove subscription
- [ ] `GET /{tenant}/pos/webhooks/{id}/deliveries` — delivery log (status, response_code, retried)
- [ ] Webhook delivery worker: on NATS event publication, fan out to all matching webhook subscriptions
- [ ] Delivery with retry: exponential backoff × 5 attempts; failures logged to `SyncFailure`
- [ ] HMAC-SHA256 signature header: `X-BengoBox-Signature` — allows receivers to verify authenticity
- [ ] Events available for subscription: `order.created`, `order.completed`, `order.refunded`, `payment.received`, `stock.low`, `shift.closed`, `eod.closed`

### Online Channel Order Ingestion
- [ ] `POST /{tenant}/pos/channels/{channel_id}/orders` — receive inbound order from marketplace (called by ordering-api or channel adapter)
- [ ] Maps external order format to `PosOrder` with `order_source = online`, `order_label = channel name`
- [ ] Creates KDS tickets automatically (same flow as in-restaurant orders)
- [ ] `ChannelSyncJob` record created for each inbound order; status tracked (received|processing|completed|failed)
- [ ] `OrderLink` record created linking `pos_order_id` ↔ `external_order_id`
- [ ] `GET /{tenant}/pos/channels` — list configured channel integrations
- [ ] `POST /{tenant}/pos/channels` — register a channel integration (name, type, credentials ref)
- [ ] `PATCH /{tenant}/pos/channels/{id}` — update channel config
- [ ] `DELETE /{tenant}/pos/channels/{id}` — remove channel

### Menu Push to Channels
- [ ] `POST /{tenant}/pos/channels/{channel_id}/sync-menu` — push current catalog to channel
- [ ] Translates `CatalogItem` to channel-specific format (Uber Eats, Glovo, generic webhook)
- [ ] Creates `ChannelSyncJob` record; async execution in background worker

### Accounting Export (Xero / QuickBooks / Sage)
- [ ] `IntegrationSetting` (existing schema) — used to store accounting platform credentials refs
- [ ] `POST /{tenant}/pos/integrations/accounting/sync` — push daily reconciliation to accounting platform
- [ ] Supports: generic journal entry CSV, Xero direct API (via XERO_CLIENT_ID/SECRET env), QuickBooks (future)
- [ ] `GET /{tenant}/pos/integrations/accounting/sync-log` — history of syncs

### ERP Integration (BengoBox ERP)
- [ ] On `pos.sale.finalized`: publish structured event to `erp.events` NATS subject with order + line details
- [ ] ERP-api subscribes and creates sales invoices automatically
- [ ] On `pos.drawer.closed`: publish cash position event to `erp.events` for ledger entry
- [ ] `GET /{tenant}/pos/integrations/erp/status` — last sync status + any failures

### Multi-Device Sync / Real-Time Cart Sharing
- [ ] `SharedCart` schema — `id, tenant_id, outlet_id, device_id, cart_data (JSON), updated_at` — enables multiple devices (e.g., waiter tablet + cashier terminal) to see the same cart
- [ ] `PUT /{tenant}/pos/carts/{cart_id}` — upsert shared cart state
- [ ] `GET /{tenant}/pos/carts/{cart_id}` — retrieve cart state
- [ ] Redis pub/sub for real-time cart updates (device subscribes to `pos:cart:{cart_id}`)

### Fiscal Device Integration (eTIMS / KRA)
- [ ] `FiscalReceipt` schema — `id, tenant_id, pos_order_id (FK), fiscal_device_id, invoice_number, cu_serial_number, qr_code_data, signed_at, status (pending|signed|failed), raw_response (JSON)`
- [ ] `POST /{tenant}/pos/fiscal/sign` — submit order to eTIMS API and receive signed receipt
- [ ] `GET /{tenant}/pos/fiscal/receipts/{order_id}` — retrieve fiscal receipt for an order
- [ ] Automatic submission on order completion if `FISCAL_ETIMS_ENABLED=true`
- [ ] KRA eTIMS compliance: invoice number sequence, QR code generation, VSCU/OSCU device support

### RBAC Additions
- [ ] New permissions: `pos.webhooks.view`, `pos.webhooks.manage`
- [ ] New permissions: `pos.channels.view`, `pos.channels.manage`
- [ ] New permissions: `pos.integrations.view`, `pos.integrations.manage`
- [ ] New permissions: `pos.fiscal.view`, `pos.fiscal.manage`

### Migration
- [ ] Add `SharedCart` ent schema
- [ ] Add `FiscalReceipt` ent schema
- [ ] Run `go generate ./internal/ent`
- [ ] Generate Atlas migration: `integrations_module`
- [ ] Update `docs/erd.md`
- [ ] Update `docs/integrations.md` with channel adapter patterns

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
