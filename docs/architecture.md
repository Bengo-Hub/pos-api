# pos-api -- Architecture

**Service**: pos-api (Go)
**Deployed**: posapi.codevertexitsolutions.com
**Port**: 4104
**Canonical tenant**: `urban-loft` | **Active outlet**: Busia

---

## Stack

| Layer | Technology |
|-------|-----------|
| Language | Go 1.22+ |
| HTTP | Chi router, shared-auth-client middleware |
| ORM | Ent (planned -- `internal/ent` not yet scaffolded) |
| Database | PostgreSQL 16 |
| Cache | Redis 7 |
| Events | NATS JetStream (stream: `pos`, subjects: `pos.*`) |
| Observability | Zap logger, Prometheus `/metrics` |
| Auth | JWT (JWKS from auth-service) + API-key for S2S |
| Event relay | Outbox table + background publisher (schema ready, publisher not wired) |
| Migrations | Planned: Atlas versioned migrations |

### Atlas migration transition

The outbox migration (`migrations/001_outbox_events.sql`) exists as raw SQL. All future domain tables must use Atlas:

1. Scaffold Ent schemas in `internal/ent/schema/`
2. Generate Atlas migrations: `atlas migrate diff --env ent`
3. Store in `migrations/` (versioned, sequential)
4. Apply via CI: `atlas migrate apply --url $DATABASE_URL`
5. No `client.Schema.Create()` in production paths

---

## Directory layout

```
pos-api/
  cmd/api/main.go              -- entry point, signal handling
  config/example.env           -- environment template
  internal/
    app/app.go                 -- bootstrap, DI, server wiring
    config/config.go           -- envconfig (POS_ prefix)
    http/
      handlers/
        health.go              -- liveness, readiness, metrics
        user.go                -- user sync, roles
        swagger.go             -- Swagger UI
      router/router.go         -- route tree, CORS, auth middleware
      docs/                    -- OpenAPI spec
    modules/
      outbox/
        publisher.go           -- outbox publisher (not yet started in app)
        repository.go          -- outbox SQL repo (pgx)
    platform/
      cache/redis.go           -- Redis client
      database/postgres.go     -- pgx connection pool
      events/nats.go           -- NATS connection + stream setup
    services/
      rbac/rbac.go             -- RBAC (in-memory roles)
      usersync/sync.go         -- auth-service user sync
    shared/logger/logger.go    -- Zap wrapper
  migrations/
    001_outbox_events.sql      -- outbox table
  docs/                        -- ERD, integrations, Superset
```

---

## Current implementation status

| Component | Status |
|-----------|--------|
| HTTP server + health endpoints | Done |
| Auth middleware (JWT + API key) | Done |
| User handler (sync, roles) | Done |
| RBAC (in-memory) | Done |
| Postgres pool | Done |
| Redis client | Done |
| NATS connection + stream | Done |
| Outbox schema | Done |
| Outbox publisher | Schema ready, not wired in app.go |
| Ent schemas (domain entities) | Not started |
| Domain handlers (orders, payments, tables) | Not started |
| Event subscribers | Not started |

---

## Multi-tenancy model

Routes are scoped by tenant:

```
/api/v1/{tenantID}/users
/api/v1/{tenantID}/pos/orders     (placeholder, returns 501)
```

Tenant metadata synced from auth-service via `auth.tenant.created` / `auth.tenant.updated` events (subscriber not yet implemented).

### Platform admin vs tenant admin

| Actor | Scope | Mechanism |
|-------|-------|-----------|
| Platform admin | Cross-tenant system config, feature overrides | `X-API-Key`, superuser JWT |
| Tenant admin | Outlet settings, tax config, pricebook, operating hours | JWT with `tenant_id` claim |
| Cashier / Staff | Order entry, shift management, cash drawer | JWT with outlet-scoped role |

---

## Multi-outlet awareness

Per `docs/erd.md`, the `outlets` table carries `tenant_id`, `tenant_slug`, `channel_type`, `timezone`. Each outlet has its own:
- `outlet_settings` (receipts, tax, service charge, hours)
- `pos_devices` and `pos_device_sessions`
- `tables` (floor plan)
- `cash_drawers`

Current MVP: single outlet (Busia) under `urban-loft`.

---

## Planned API surface

### Public

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/healthz` | Liveness |
| GET | `/readyz` | Readiness (Postgres, Redis, NATS) |
| GET | `/metrics` | Prometheus |
| GET | `/v1/docs/*` | Swagger UI |

### Authenticated (implemented)

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/api/v1/{t}/users` | Create/sync user |
| GET | `/api/v1/{t}/users/me/roles` | Current user roles |
| GET | `/api/v1/{t}/roles` | List roles |

### MVP domain endpoints (to implement)

| Domain | Endpoints | Priority |
|--------|-----------|----------|
| Orders | `POST /orders`, `GET /orders`, `GET /orders/{id}`, `PUT /orders/{id}/status` | P0 |
| Order lines | `POST /orders/{id}/lines`, `PUT /lines/{lineId}`, `DELETE /lines/{lineId}` | P0 |
| Payments | `POST /orders/{id}/payments`, `GET /orders/{id}/payments` | P0 |
| Cash drawers | `POST /drawers/open`, `POST /drawers/close`, `GET /drawers/current` | P0 |
| Tables | `GET /tables`, `POST /tables/{id}/assign`, `POST /tables/{id}/release` | P1 |
| Shifts | `POST /shifts/open`, `POST /shifts/close`, `GET /shifts/current` | P1 |
| Catalog sync | `GET /catalog/items`, `GET /catalog/categories` | P0 |
| Kitchen tickets | `POST /orders/{id}/ticket`, `PUT /tickets/{id}/status` | P1 |

---

## Database schema (planned)

From `docs/erd.md`, the full schema includes:

**Core**: `tenants`, `outlets`, `outlet_settings`, `pos_devices`, `pos_device_sessions`

**Users/Roles**: `pos_roles`, `user_pos_roles`, `license_usage_snapshots`, `feature_overrides`

**Catalog**: `catalog_items` (synced from inventory/ordering), `price_books`, `price_book_items`, `modifier_groups`, `modifiers`

**Orders**: `pos_orders`, `pos_order_lines`, `pos_line_modifiers`, `pos_order_events`, `tables`, `table_assignments`, `bar_tabs`, `bar_tab_events`

**Payments**: `tenders`, `pos_payments`, `cash_drawers`, `cash_drawer_events`, `pos_refunds`

**Promotions**: `promotions`, `promotion_rules`, `promotion_applications`, `gift_cards`, `gift_card_transactions`

**Inventory**: `stock_consumption_events`

**Events**: `outbox_events` (implemented)

---

## Event architecture

### NATS stream: `pos` (subjects: `pos.*`)

**Published (via outbox)**:

| Event | Trigger |
|-------|---------|
| `pos.order.created` | New POS order |
| `pos.order.ready` | Order ready for pickup/serve |
| `pos.order.completed` | Order fulfilled and paid |
| `pos.payment.initiated` | Payment intent created |
| `pos.settlement.requested` | End-of-day settlement |
| `pos.stock.adjustment.requested` | Manual stock adjustment |
| `pos.cash.drawer.alert` | Drawer variance exceeds threshold |

**Consumed**:

| Event | Action |
|-------|--------|
| `auth.tenant.created` | Initialize tenant + outlet |
| `auth.outlet.created` | Register outlet |
| `inventory.catalog.updated` | Refresh catalog cache |
| `inventory.stock.updated` | Update item availability |
| `inventory.stock.low` | Flag low-stock items |
| `treasury.payment.success` | Mark payment complete |
| `treasury.payment.failed` | Mark payment failed |
| `logistics.task.assigned` | Update order with driver info |
| `logistics.task.completed` | Mark delivery complete |

---

## MVP scope (March 17, 2026)

### Must-have (P0)

- Ent schema scaffolding for core tables (outlets, orders, lines, payments, catalog, drawers)
- Atlas migration generation and CI pipeline
- Order CRUD (create, add lines, modifiers, status flow)
- Catalog sync endpoint (read from inventory-service cache)
- Payment recording (cash, card, mobile money -- via treasury-api)
- Cash drawer open/close with float tracking
- Outbox publisher wiring (start in app.go)
- Event subscribers for auth, inventory, treasury
- Stock consumption event publishing on order completion

### Nice-to-have (P1)

- Table management (assign/release)
- Kitchen ticket generation
- Shift management (open/close)
- Bar tab support

### Post-MVP

- Offline queue (Redis) for network outage resilience
- Barcode scanner integration
- Fiscal printer support
- Gift card and promotion engine
- Multi-pricebook support
- KDS (Kitchen Display System) WebSocket stream
