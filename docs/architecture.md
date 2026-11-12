# POS Service - Architecture Overview

**Last updated:** 2026-05-20  
**Audit note (2026-05-20):** Outlet use-case gating added (RequireUseCase middleware, OutletSetting per-outlet toggles). Terminal JWT and PIN auth response now embed outlet_use_case + is_hq_user claims. Seed restructured for codevertex-demo (6 outlets) + urban-loft (hospitality only). Demo staff moved from urban-loft to codevertex-demo.  
**Previous audit (2026-05-09):** Payment flow architecture section added (pos-api as thin treasury client). eTIMS ownership corrected to treasury-api. Event publisher reality gap documented (pos.sale.finalized not yet in publisher.go).

## Design Philosophy

The POS service is the **transactional backbone** of Codevertex — a multi-tenant, multi-vertical point-of-sale platform built for the Kenyan market. It follows a **Core + Vertical Modules** architecture: a shared transactional core (orders, payments, RBAC, devices, KDS) with pluggable vertical-specific modules (hotel, retail, service).

> **Pharmacy/clinical workflow moved to hospital-service (2026-08-29).** pos-api's `pharmacy` use
> case (prescriptions, OPD Records/Triage/Examination/Lab, controlled-substance register) was
> decisively migrated to `hospital-service` (Codevertex Afya) — see
> `hospital-service/hospital-api/docs/migration-pos-pharmacy.md`. pos-api owns no pharmacy/clinical
> schema, handlers, or routes going forward; valid `use_case` values are exactly `hospitality`,
> `quick_service`, `retail`, `services`.

The service adapts UI and business workflows based on the `use_case` configured per Outlet, which can override the Tenant default.

### Supported Use Cases

| Use Case | Example Businesses | Module Scope |
|----------|-------------------|--------------|
| `hospitality` | Restaurant, bar, café, food court | Orders, tables, KDS, bar tabs, hotel module |
| `quick_service` | Food kiosk, fast food counter | Orders, KDS, queue management |
| `retail` | Supermarket, mini-mart, hardware, electronics | Orders, barcode scanning, layaway, scale |
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
| `payments` | Payment recording, tender routing, treasury S2S (card/M-Pesa) | ✅ Intent endpoint registered; ❌ NATS subscriber not wired |
| `promotions` | Promo code validation, percentage/fixed/BOGO discounts | ✅ Complete |
| `rbac` | Role-based access, 126 permissions, 8 system roles | ✅ Complete |
| `catalog` | Menu items, categories, price books, modifiers, barcode lookup | ✅ Complete |
| `tables` | Floor plan sections, table assignment/release | ✅ Complete |
| `kds` | Ticket creation, station routing, item-level status | ✅ Complete |
| `hotel` | Rooms, guests, folio, facilities, bookings | ✅ Schema+handlers done |
| `closings` | DailyClosing entity, daily-close + list endpoints | ✅ Handler done; gated by shift_reports feature |
| `returns` | POSReturn + POSReturnLine, create/list/approve endpoints | ✅ Handler done; treasury refund call not wired |
| `receipt` | Receipt data endpoint | ✅ Handler done; PDF format pending |
| `layaway` | LayawayPlan + LayawayPayment, create/list/get/payment/cancel | ✅ Complete |
| `scale` | WeighingScaleReading, create + list readings | ✅ Complete |
| `appointments` | List/create/get/update/availability; gated by services use_case | ✅ Core done; action endpoints (check-in/start/complete/cancel) pending |
| `staff_schedule` | 7-day schedule upsert per staff member | ✅ Complete |
| `commissions` | CommissionRecord list/get | ✅ Basic done; rules and payout pending |
| `loyalty` | LoyaltyProgram + LoyaltyAccount + LoyaltyTransaction, earn/redeem | ✅ Core done |
| `reports` | Sales-summary, refund-summary, daily-breakdown | ✅ Core done; EOD, shift, export pending |
| `webhooks` | WebhookSubscription + WebhookDelivery, CRUD + delivery log | ✅ CRUD done; delivery worker pending |
| `online_orders` | Pickup queue, mark-ready, mark-collected | ✅ Done; NATS KDS subscriber pending |
| `pin_auth` | Terminal PIN login, set PIN, staff list, auth/me | ✅ Complete |
| `inventory` | Catalog sync subscriber, stock consumption S2S | ❌ NATS subscriber not wired |
| `treasury` | Payment intent S2S, NATS success/failed consumers | ❌ NATS subscribers not wired |

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

## Outlet Use-Case Gating

Routes are gated at both the middleware and sidebar level based on `outlet.use_case`. This is separate from RBAC — use-case gating prevents hospitality-specific features from being accessible on a retail or services outlet.

### Middleware (`internal/http/middleware/use_case.go`)

| Middleware | Gates | Allowed Use Cases |
|------------|-------|------------------|
| `RequireUseCase("hospitality")` | `/tables/*`, `/bar-tabs/*`, `/hotel/*` | hospitality only |
| `RequireUseCase("hospitality", "quick_service")` | `/kds/*` | hospitality, quick_service |
| `RequireKDSEnabled(client)` | `/kds/*` | outlet must have `enable_kds=true` |
| `RequireAppointmentsEnabled(client)` | `/appointments/*` | outlet must have `enable_appointments=true` |

Platform owners and HQ users (`is_hq_user=true`) bypass all use-case gates.

### Outlet scope of reads (`?outlet_id=`)

`OutletContextMiddleware` resolves the request's outlet (X-Outlet-ID, else the token's outlet, else
the HQ outlet) and refuses a header outlet a non-HQ user is not assigned to. Reads that also take
`?outlet_id=` resolve it with `outletmw.RequestOutletScope` (2026-11-03): the All-Sales list,
summary and export, the dashboard KPI summary, and the analytics, register, KDS, hotel and PDF
reports. `all` spans every outlet for HQ-level callers only (`CanAccessAllOutlets`); a named
outlet counts only when the caller may see it (`CanViewOutlet`: HQ caller, their resolved outlet,
an assigned outlet or the HQ outlet); anything else falls back to the request's outlet. Since the
outlet context always resolves to an outlet, a report is tenant-wide only with `outlet_id=all`.

### OutletSetting (`outlet_settings` table)

Per-outlet configuration stored in `outlet_settings`. Fields:
- `pin_login_message` — shown on PIN login page
- `display_mode` — `card` | `list`
- `default_view` — `tables` | `catalog` | `orders`
- `enable_kds` — boolean
- `enable_appointments` — boolean
- `show_barcode_scanner` — boolean (auto-true for retail outlets)

Settings are cached in-memory with a 5-minute TTL to avoid DB hits on every request.

### Seed Outlets

| Tenant | Outlet | Code | Use Case | Is HQ |
|--------|--------|------|----------|-------|
| urban-loft | Urban Loft Cafe Busia | BUSIA | hospitality | ✅ |
| codevertex-demo | Demo Grand Hotel & Restaurant | HOSP | hospitality | ✅ |
| codevertex-demo | Demo Tech Store | RETAIL | retail | ❌ |
| codevertex-demo | Demo Express Kiosk | QSR | quick_service | ❌ |
| codevertex-demo | Demo Beauty & Wellness | SVC | services | ❌ |
| codevertex-demo | Demo Logistics Hub | LOGIS | logistics | ❌ |

Demo staff (PIN=`1234`) are seeded under `codevertex-demo` only. Urban-loft has no demo staff — it is a real client.

---

## RBAC & Authorization

- **Format**: `pos.{module}.{action}` — e.g., `pos.orders.view`, `pos.hotel.manage`
- **14 modules**: orders, payments, catalog, outlets, devices, sessions, cash_drawers, tables, gift_cards, price_books, modifiers, channels, config, users
- **9 actions per module**: add, view, view_own, change, change_own, delete, delete_own, manage, manage_own (126 total)
- **System roles (seeded per tenant)**: `pos_admin`, `store_manager`, `cashier`, `waiter`, `viewer`, `receptionist`, `kitchen`, `bar`
- **Endpoints**: 7 under `/{tenant}/rbac/`
- **Rate limiting**: DB-driven configs (`rate_limit_configs`) — per-IP, per-tenant, per-user, global

### Role scope, tiers and the permission ceiling (updated 2026-11-12)

- **A tenant uses only shared roles and its own.** A role with no tenant is a shared system role. A role with a tenant belongs to that tenant only. Assigning a role (`POST /rbac/assignments`) answers 404 unless the role is shared or this tenant's own, and the repository refuses another tenant's role whoever calls it. Every reader of role assignments (the PIN and `/auth/me` permission set, the assigned role list, the route permission check and the staff tier guards) counts only assignments in the request's tenant whose role is shared or that tenant's. An assignment row that names another tenant's role grants nothing. Before this, a person who ran one tenant could build an admin grade role there and, as a manager in another tenant, assign it to themselves.
- **Reserved codes.** A custom role (`POST /rbac/roles`) may not use the code of a system or platform role or an alias of one: admin, administrator, owner, superuser, superusers, super admin, pos admin, tenant admin, system admin, platform admin, platform owner, manager, store manager, outlet manager, supervisor, cashier, staff, auth's self-service roles (member, customer, student, parent, driver) and the other customer-side names (rider, learner, occupant, viewer), any admin or manager tier code, and the code of any shared system role such as waiter or kitchen. I compare codes without case, spaces, hyphens or underscores. A tenant row with one of these codes would otherwise win over the shared role for everyone who holds that code. Tenant changes to a system role still go through the permission matrix, which makes a tenant copy and is admin only.
- **Self-service roles get nothing in POS.** In a business tenant, anyone can join without an invite only as a customer, student, parent or driver, so those are never staff roles here. A person whose token holds only those maps to no POS role, gets no POS permissions, no default role assignment and no staff record at sign-in, and those names are never read as POS role codes. Someone holding a real staff role next to one (customer plus cashier, say) is unaffected. Before this they fell back to cashier like any unknown role. "member" is different: since 2026-11-10 a plain member of a business tenant is invite only, and auth shows an invited member with no roles as "member", so it is invited staff and still maps to cashier. It stays reserved as a tenant role code.
- **Tiers count permissions.** Staff, manager and admin tiers come from the role code for the shared system roles. Every other role, a custom role or a tenant's copy of a system role, takes the higher of its code tier and its permission tier: it is manager tier when it carries a permission no shared staff tier role has, and admin tier when it carries one no shared manager tier role has. The guards that use tiers (who may assign a role, the staff bind check, PIN resets, and clearing a PIN when an assignment raises someone) all use this.
- **Callers below admin work with staff tier roles only.** A manager may create, edit, rename, assign, revoke and delete staff tier roles. They may not edit, rename or delete a role that is already above staff tier, may not make an edit that would lift a role above staff tier (even with permissions they hold, so a role they already handed out cannot become a manager role behind its holders' PINs), may not assign a manager or admin tier role, and may not revoke someone's assignment of one.
- **Ceiling for callers below admin.** A manager may not leave a role carrying a permission they do not hold themselves (`PUT /rbac/roles/{id}/permissions`), and may not assign a role that does (`POST /rbac/assignments`). "Hold" uses the same resolution as route permissions: the token's permissions, then per-user assignments, then the POS role mapped from their global roles. Admins, owners and platform owners are not limited. A permission matrix edit on a role this tenant cannot see answers 404 instead of skipping the system role guard.
- **Not covered yet.** When an admin raises an existing role's permissions, the PINs of people who already hold it are not cleared. Managers can no longer raise a role at all.
- **Sign-in provisioning stays in the token's tenant.** When someone uses POS with a token, the staff record I make sure of is in that token's tenant, with that token's roles, never in the tenant where pos first saw the person. A token for tenant B can no longer recreate a record the person was removed from in tenant A, or give a member of A an admin record in A because they run B. Provisioning runs only when the token says the person may use POS. A token issued before a revoke can still carry that until it expires.

---

## Authentication

### SSO Login (Browser / Manager)
- JWT validation via `shared/auth-client` (JWKS RS256, audience `codevertex`)
- Used by: managers, admins, office staff

### Terminal PIN Login (POS Terminals)
- Status: ✅ Implemented
- `StaffMember` table: `{tenant_id, outlet_id, user_id, name, role, pin_hash (bcrypt), ...}`
- `POST /{tenant}/pos/auth/pin` — validates PIN, issues short-lived 4-hour terminal JWT
  - Response includes: `outlet_use_case`, `is_hq_user`, full permissions array
- `POST /{tenant}/pos/auth/pin/set` — manager sets/resets staff PIN (rules below)
- Terminal JWT claims mirror SSO JWT: `outlet_id`, `outlet_code`, `outlet_use_case`, `is_hq_user`
- Required for: waiters, cashiers, kitchen, bar staff, receptionists on dedicated terminals

#### Who may set a PIN, and on whose staff record (updated 2026-11-11)

A staff record acts for the person it is bound to. It inherits that person's POS role assignments, and an auth sync later moves its role to theirs. A PIN is a way to sign in as that record, so these rules hold together:

- **Tiers.** Staff roles fall into three tiers: everyone else, the manager tier (manager, store manager, supervisor) and the admin tier (admin, owner, superuser and the platform roles). "Admin level" below means the manager or admin tier, the same notion the earlier guards use.
- **Binding a staff record.** When someone below admin creates a staff record (`POST /{tenant}/pos/staff`), I refuse it with 403 if the person, named by `user_id` or by email, is admin level in auth for this tenant, holds an admin level POS role assignment here, already has an admin level staff record here, or is a platform owner. The auth check runs before auth is asked to provision anyone, and if auth cannot answer I refuse with 502 rather than guess. An admin, owner or platform owner may bind anyone who belongs to the tenant.
- **Raising a role clears the PIN.** When a staff edit, an auth sync (`auth.user.created`, `auth.user.updated`) or a POS role assignment (`POST /rbac/assignments`, and the sign-in path that assigns a default role) moves a person into a higher tier, I clear the PIN hash, fast hash, failed count and lockout of their staff record in that tenant, and log only the tenant and staff ids. The person then sets a new PIN. For an assignment the bar is the highest of the record's own role and every role already assigned to them. A move within a tier, or down, keeps the PIN. `auth.user.pin_set` is different: it carries the person's new PIN with the role, so that PIN applies.
- **The auth sync stays in its tenant.** A person can belong to several tenants. `auth.user.updated` and `auth.user.pin_set` carry one tenant's roles and PIN, so I resolve that tenant from `tenant_slug` (then the event's tenant id) and touch only the person's staff record in it. Before this, the record was found by the tenant where pos first saw the person, or by user id alone, so a cashier in one tenant who ran their own tenant could set an admin role and PIN that landed on their cashier record. When the event carries a `tenant_slug` that pos cannot resolve yet, and its tenant id is not one pos holds, the handler returns an error so the message is redelivered instead of being lost. Failed auth events are retried after 2, 10, 30 and 60 seconds, five deliveries in all; the last failure logs "auth user event dropped after max deliveries" with the event, user and tenant ids. With no slug and an unknown tenant id, or no record for the person, `updated` changes nothing and logs ids. `pin_set` then creates the record in that tenant only, so a PIN that arrives before `auth.user.created` is not lost. When an event's services list leaves POS out, I remove the person's staff record, staff outlets and role assignments in that tenant only. Their records in other tenants, role assignments included, stay usable, and their pos user record goes only when no tenant still has a staff record, role assignment or outlet role for them (a person who uses POS through sign-in only has no staff record but keeps their assignments elsewhere). A full account deletion (`auth.user.deleted`) still removes them everywhere.
- **Effective tier.** For editing, deactivating and setting the PIN of a record, and for the weak PIN refusal, a record counts as admin level when its own role is, or when its person holds an admin level POS role assignment in that tenant. Only an admin may do those things to such a record. Anyone may still change their own PIN.
- **Not covered by the bind check.** auth-api's member list holds only active memberships, so a manager can still bind someone whose admin membership is suspended or invited. When it becomes active again, the `auth.user.updated` event raises the record and clears its PIN. The PIN reset check does not look at the person's auth membership tier or platform owner status. Such records only come from an admin's bind or from the sync, and the sync clears the PIN when it raises them.
- **Follow up.** For an existing account that is not yet a member, auth-api answers the S2S member add with 202 and an invitation instead of a member. pos treats that as a failure today and answers 502 "could not provision", for admins too. It should say "invitation sent, no staff record yet". A manager adding staff by email can trigger such invitation emails.

#### Manager cards (updated 2026-11-11)

A manager card token is accepted only on the tenant it was minted for (its subject) and only when the card issuer minted it. A card from one tenant can no longer approve voids, discounts or refunds in another tenant where the same person is a manager.

#### Public PIN profile list and sign in by staff id (updated 2026-11-11)

- `GET /{tenant}/pos/auth/pin/profile` is public and returns only `staff_id`, `name`, `initials`, `tenant_id` and `has_pin` for each active staff member. It no longer returns the auth user id, the role or the home outlet.
- `POST /{tenant}/pos/auth/pin` takes `staff_id` and `pin`. The login responses (`/auth/pin` and `/auth/pin/identify`) now also return `user.staff_id`, so the terminal can key its offline PIN cache by staff id.
- `user_id` on `POST /{tenant}/pos/auth/pin` still works for one release so older terminals keep signing in. I remove it in the release after pos-ui's staff id change has shipped everywhere.

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
| `DEFAULT_CURRENCY` | `KES` |
| `ORDER_PREFIX` | Order number prefix (e.g., `BNG`) |
| `INTERNAL_SERVICE_KEY` | Single shared S2S key (`X-API-Key` header) |
| `INVENTORY_SERVICE_URL` | `https://inventoryapi.codevertexafrica.com` |
| `TREASURY_SERVICE_URL` | `https://booksapi.codevertexafrica.com` |
| `NOTIFICATIONS_SERVICE_URL` | `https://notificationsapi.codevertexafrica.com` |
| `ORDERING_SERVICE_URL` | `https://orderingapi.codevertexafrica.com` |

---

## Document Numbering

Per-tenant, per-doc-type document numbers are minted through the `DocumentSequence` machinery
(`internal/modules/documents`), mirroring treasury-api and inventory-api.

- **Numeric by default.** A new tenant's sequences seed with an empty prefix and empty date
  format, so `formatNumber` emits just the zero-padded counter (e.g. `000001`). A tenant can opt
  any doc type into a prefixed/dated format (e.g. `POS-260625-000001`) from **Settings → Document
  Numbering** (pos-ui); `SuggestedPrefixes` supplies the pre-fill hint (order→POS, return→RET,
  reversal→REV, repair→JOB). There is no separate receipt doc type — a receipt always carries its
  order's own number (uniform order-number/receipt-number, see handlers.ReceiptHandler.
  ensureReceiptNumber).
- **Doc types:** `order`, `pos_return`, `pos_reversal`, `repair_job`.
- **Wiring:** order, return, reversal, and repair-job numbers all route through the sequence, each
  keeping its legacy ad-hoc format as a fallback when the sequence service is unavailable. The
  receipt printed for an order simply reads that order's number back — no counter of its own.
- **API:** `GET/PUT /api/v1/{tenantID}/pos/document-sequences` (+ `/{docType}/preview`); `PUT` is
  gated by `pos.config.change` / `pos.config.manage`. Backed by the `document_sequences` table
  (unique on `(tenant_id, doc_type)`), migration `20260719180642_add_document_sequences.sql`.

---

## Sprint Status Summary

| Sprint | Title | Status |
|--------|-------|--------|
| 1 | Foundation (Auth, RBAC, Devices, PIN Auth) | ✅ Complete |
| 2 | Orders, Catalog, Payments, Tables | ✅ Complete |
| 3 | Hotel Module | ✅ Complete |
| 4 | KDS & Bar Display | ✅ Complete |
| 5 | ERP Gaps (DailyClosing, Returns, Receipt) | ✅ Substantially complete |
| 6 | Inventory & Treasury Wiring | 🟡 S2S clients referenced; NATS subscribers missing |
| 7 | Retail Module (Layaway, Barcode, Scale, Serial) | ✅ Core delivered |
| 8 | ~~Pharmacy Module~~ — decisively removed 2026-08-29, migrated to hospital-service | N/A |
| 9 | Service Business Module (Appointments, Schedules, Commissions) | ✅ Core delivered |
| 10 | Loyalty Programs, Accounts, Earn/Redeem | ✅ Core delivered |
| 11 | Reporting — Sales/Refund/Daily KPIs | ✅ Core KPIs delivered |
| 12 | Webhook CRUD + Delivery Schema | 🟡 CRUD done; delivery worker missing |
| 13 | Online Order Pickup Endpoints | 🟡 Pickup done; NATS KDS subscriber missing |
