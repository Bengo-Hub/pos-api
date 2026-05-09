# POS Service - Architecture Overview

**Last updated:** 2026-05-09  
**Audit note (2026-05-09):** Payment flow architecture section added (pos-api as thin treasury client). eTIMS ownership corrected to treasury-api. Event publisher reality gap documented (pos.sale.finalized not yet in publisher.go). Schema field name aliases clarified (status vs payment_status, external_reference vs provider_reference).

## Design Philosophy

The POS service is the **transactional backbone** of BengoBox — a multi-tenant, multi-vertical point-of-sale platform built for the Kenyan market. It follows a **Core + Vertical Modules** architecture: a shared transactional core (orders, payments, RBAC, devices, KDS) with pluggable vertical-specific modules (hotel, retail, pharmacy, service).

The service adapts UI and business workflows based on the `use_case` configured per Outlet, which can override the Tenant default.

### Supported Use Cases

| Use Case | Example Businesses | Module Scope |
|----------|-------------------|--------------|
| `hospitality` | Restaurant, bar, café, food court | Orders, tables, KDS, bar tabs, hotel module |
| `quick_service` | Food kiosk, fast food counter | Orders, KDS, queue management |
| `retail` | Supermarket, mini-mart, hardware, electronics | Orders, barcode scanning, layaway, scale |
| `pharmacy` | Community pharmacy, hospital dispensary | Orders, prescriptions, lot/batch, NHIF |
| `services` | Salon, spa, clinic, car wash | Orders, appointments, packages, commission |

---

## Layer Overview

### HTTP Layer (`internal/http/`)
- `router/router.go` — Route registration per tenant, RBAC middleware
- `handlers/` — One handler per module: orders, payments, tables, kds, hotel, drawer, etc.
- Middleware: `RequireAuth` (JWT/API-key), `RequirePermission`, rate limiting (DB-driven)

### Service Layer (`internal/modules/`)

| Module | Service | Status |
|--------|---------|--------|
| `orders` | Order creation, state machine (draft→open→completed/cancelled/voided→refunded) | ✅ Complete |
| `payments` | Payment recording, tender routing, treasury S2S (card/M-Pesa) | ✅ Entity done; ❌ S2S not wired |
| `promotions` | Promo code validation, percentage/fixed/BOGO discounts | ✅ Complete |
| `rbac` | Role-based access, 126 permissions, 5 system roles | ✅ Complete |
| `catalog` | Menu items, categories, price books, modifiers | ✅ Complete |
| `tables` | Floor plan sections, table assignment/release | ✅ Complete |
| `kds` | Ticket creation, station routing, item-level status | ✅ Complete |
| `hotel` | Rooms, guests, folio, facilities, bookings | ✅ Schema+handlers done |
| `inventory` | Catalog sync, stock consumption events | ❌ NATS subscriber not wired |
| `treasury` | Payment intent creation, NATS event consumption | ❌ Not wired |
| `retail` | Layaway, weight-based pricing, serial number | 🔴 Not started |
| `pharmacy` | Prescriptions, lot/batch, drug interactions, NHIF | 🔴 Not started |
| `services` | Appointments, packages, commission | 🔴 Not started |
| `loyalty` | Points, tiers, rewards | 🔴 Not started |
| `reporting` | Daily close, EOD, KRA exports | 🔴 Not started |

### Data Layer (Ent ORM)
- All schemas in `internal/ent/schema/`
- Atlas migrations in `db/migrations/`
- All schema additions trigger `go generate ./internal/ent` + `atlas migrate diff`

### Event Bus (NATS JetStream)
- Outbox pattern via `outbox_events` table + background publisher
- Publisher: `internal/platform/events/publisher.go`
- Subscribers: `internal/platform/events/subscribers.go` (partial — see Integrations doc)

**Publisher reality (2026-05-09):** `publisher.go` currently only defines `PublishOrderCreated`, `PublishOrderStatusChanged`, `PublishPaymentRecorded`. The planned events `pos.sale.finalized` and `pos.drawer.closed` are **not yet implemented** and must be added as part of Sprint 6 before inventory and treasury NATS subscriptions are of any value.

---

## Data Authority

| Domain | This Service Owns | References (Never Stores) |
|--------|------------------|--------------------------|
| Sales transactions | `pos_orders`, `pos_order_lines`, `pos_payments`, `pos_refunds` | User IDs from auth-api |
| Shift lifecycle | `pos_device_sessions`, `cash_drawers` | Outlet/tenant from auth-api |
| POS catalog | `catalog_items` (read cache), `price_books`, `modifiers` | Item master from inventory-api |
| Hotel | `rooms`, `room_guests`, `room_folio_items`, `facilities`, `facility_bookings` | — |
| KDS | `kds_stations`, `kds_tickets` | — |
| RBAC | `pos_permissions`, `pos_role_v2s`, `pos_role_permissions`, `pos_user_role_assignments` | — |

---

## RBAC & Authorization

- **Format**: `pos.{module}.{action}` — e.g., `pos.orders.view`, `pos.hotel.manage`
- **14 modules**: orders, payments, catalog, outlets, devices, sessions, cash_drawers, tables, gift_cards, price_books, modifiers, channels, config, users
- **9 actions per module**: add, view, view_own, change, change_own, delete, delete_own, manage, manage_own (126 total)
- **System roles (seeded per tenant)**: `pos_admin`, `store_manager`, `cashier`, `waiter`, `viewer`, `receptionist`, `kitchen`, `bar`
- **Endpoints**: 7 under `/{tenant}/rbac/`
- **Rate limiting**: DB-driven configs (`rate_limit_configs`) — per-IP, per-tenant, per-user, global

---

## Authentication

### SSO Login (Browser / Manager)
- JWT validation via `shared/auth-client` (JWKS RS256, audience `codevertex`)
- Used by: managers, admins, office staff

### Terminal PIN Login (POS Terminals) — Sprint 10
- Status: ❌ Not implemented
- `POSStaffPin` table: `{tenant_id, user_id, pin_hash (bcrypt), is_active, last_used_at}`
- `POST /{tenant}/pos/auth/pin` — validates PIN, issues short-lived 4-hour terminal JWT
- `POST /{tenant}/pos/auth/pin/set` — manager sets/resets staff PIN (requires `pos.staff.manage`)
- Required for: waiters, cashiers, kitchen, bar staff, receptionists on dedicated terminals

---

## Kenya-Specific Requirements

### KRA eTIMS Compliance — ❌ Not Implemented (Sprint 12, partial)
Mandatory since January 2024. Without this, clients cannot issue legal tax receipts.

**eTIMS is owned by treasury-api.** pos-api does NOT call the KRA eTIMS API and does NOT store a `FiscalReceipt` entity. The env vars `ETIMS_URL`, `ETIMS_CU_SERIAL`, `ETIMS_API_KEY` belong in treasury-api only.

**pos-api role in eTIMS compliance:**
1. Publishes `pos.sale.finalized` on order completion → treasury-api signs the invoice via KRA eTIMS API
2. Subscribes to `treasury.fiscal.signed` NATS event → writes `etims_invoice_number` + `etims_qr_code_url` to `pos_orders`
3. Returns those fields in `GET /{tenant}/pos/orders/{id}` so pos-ui can render the QR code on receipts

**Sprint 12 implementation needed (pos-api):**
- Add `etims_invoice_number` (nullable string) and `etims_qr_code_url` (nullable string) to `pos_orders` Ent schema
- Add NATS subscriber for `treasury.fiscal.signed`
- Ensure order response includes both fields

See [integrations.md — eTIMS Ownership ADR](integrations.md) for full rationale.

### M-Pesa STK Push — ⚠️ Partial (via Treasury)
- M-Pesa processed via treasury-api (Daraja API integration there)
- pos-api creates payment intent → treasury-api triggers STK push → NATS callback
- **Gap**: Treasury S2S call not yet wired in `payments.Service.RecordPayment()` (Sprint 6)
- **Gap**: NATS subscriber for `treasury.payment.success/failed` not wired in pos-api (Sprint 6)

### Payment Architecture — pos-api as Thin Client

pos-api is a **thin client** of treasury-api for all non-cash payment tenders (card, M-Pesa, room charge at check-out). The architecture is:

```
pos-ui → POST /{tenant}/pos/orders/{id}/payments
  Cash:        pos-api records immediately, auto-completes order
  M-Pesa/Card: pos-api → treasury-api POST /s2s/{tenant}/payments/intents
                       ← { intent_id, checkout_request_id | authorization_url }
               pos-api stores intent_id in pos_payments.external_reference
               pos-api returns { status: "pending", intent_id } to pos-ui
               pos-ui polls GET /{tenant}/pos/orders/{id}/payments every 3s
               treasury.payment.success NATS → pos-api sets pos_payments.status = "completed"
               treasury.payment.failed  NATS → pos-api sets pos_payments.status = "failed"
```

**Schema alignment (actual code, 2026-05-09):**
- `pos_payments.status` — valid values: `pending`, `completed`, `failed`, `refunded` (field name in Ent schema)
- `pos_payments.external_reference` — stores treasury `intent_id` (field name in Ent schema)
- The integrations doc uses `payment_status` and `provider_reference` as aliases — these refer to the same fields above

**What pos-api does NOT own:**
- Payment gateway credentials (Daraja/Paystack) → treasury-api only
- Ledger entries, settlement reconciliation → treasury-api only
- eTIMS invoice signing → treasury-api only (see eTIMS section above)

### Offline Resilience
- POS terminals use local cache (SQLite/IndexedDB) during outages
- Background workers sync offline transactions on reconnect
- Cash-only payments enforced offline (card/mobile require network connection)

---

## Configuration

| Env Var | Purpose |
|---------|---------|
| `TAX_RATE_PERCENT` | Default VAT rate (16% Kenya) |
| `DEFAULT_CURRENCY` | `KES` |
| `ORDER_PREFIX` | Order number prefix (e.g., `BNG`) |
| `INTERNAL_SERVICE_KEY` | Single shared S2S key (`X-API-Key` header) |
| `INVENTORY_SERVICE_URL` | `https://inventoryapi.codevertexitsolutions.com` |
| `TREASURY_SERVICE_URL` | `https://booksapi.codevertexitsolutions.com` |
| `NOTIFICATIONS_SERVICE_URL` | `https://notificationsapi.codevertexitsolutions.com` |
| `ORDERING_SERVICE_URL` | `https://orderingapi.codevertexitsolutions.com` |

---

## Sprint Status Summary

| Sprint | Title | Status |
|--------|-------|--------|
| 1 | Foundation (Auth, RBAC, Devices) | ✅ Complete |
| 2 | Orders, Catalog, Payments, Tables | ✅ Complete |
| 3 | Hotel Module | ✅ Schema + handlers done |
| 4 | KDS & Bar Display | ✅ Handlers done |
| 5 | ERP Gaps (Daily Close, Returns) | 🟡 Planned |
| 6 | Inventory & Treasury Wiring | 🟡 NATS subscribers needed |
| 7 | Retail Module (Layaway, Barcode, Scale) | 🔴 Not started |
| 8 | Pharmacy Module | 🔴 Not started |
| 9 | Service Business Module | 🔴 Not started |
| 10 | Loyalty & Advanced Promotions | 🔴 Not started |
| 11 | Reporting & Analytics | 🔴 Not started |
| 12 | Integrations, Webhooks, KRA eTIMS | 🔴 Not started |
| 13 | Online Ordering → KDS Bridge | 🔴 Not started |
