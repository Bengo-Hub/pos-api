# POS Service

The POS Service delivers a configurable, multi-tenant point-of-sale backend for cafés/bars, retail outlets, kitchens, kiosks, and ecommerce counters within the BengoBox ecosystem, built on the shared `tenant_slug` and outlet registry used by cafe-backend, inventory, logistics, treasury, and auth services.

## Core Capabilities

**Scope**: This service handles **OVER-THE-COUNTER**, **PICKUP**, and **DINE-IN** orders. Online delivery orders are handled by the **Ordering Service**.

- **Outlet Operations**: Device provisioning, session control, cashier workflows, offline-first architecture
- **Order Fulfillment**: Walk-in orders, customer pickup orders (online or walk-in), dine-in/table service, bar tabs
- **Kitchen Operations**: Kitchen ticket management, prep station workflows, order ready notifications
- **Cash Management**: Cash drawer operations, shift reconciliation, cash variance tracking
- **POS RBAC**: Cashier, manager, and supervisor roles with granular permissions
- **Catalog Sync**: Real-time catalog synchronization with ordering and inventory services
- **Table Management**: Dine-in order tracking, table assignments, split bills, tab management
- **Kiosk Flows**: Self-service kiosk ordering for quick-service restaurants
- **Payment Integration**: Cash, card, mobile payments via treasury service
- **Settlement Reporting**: End-of-day reports, payment reconciliation, revenue accounting

## Technology Stack

- Go 1.22+, Ent ORM, PostgreSQL, Redis.
- REST APIs using `chi`, optional ConnectRPC/gRPC for streaming updates.
- Swagger/OpenAPI documentation, WebSocket support for real-time dashboards.
- Observability with zap logging, Prometheus metrics, OpenTelemetry tracing.

## Local Setup

```shell
cp config/example.env .env
make deps
docker compose up -d postgres redis
go generate ./internal/ent
go run ./cmd/server
```

Default HTTP port: `4104` (`POS_HTTP_PORT` override).

## Repository Layout

- `cmd/` – service entrypoints (`server`, `migrate`, `seed`, `worker`).
- `internal/app` – configuration and dependency wiring.
- `internal/ent` – Ent schemas and generated code.
- `internal/modules` – domains (catalog, orders, tendering, licensing, integrations).
- `docs/` – ERD, ADRs, integration guides.

## Integrations

- **Ordering Service**: Receive online-for-pickup orders, notify when pickup ready, catalog sync
- **Inventory Service**: Stock consumption tracking, low-stock alerts, BOM depletion via webhooks
- **Logistics Service**: Curbside pickup coordination, delivery handoff for pickup orders
- **Treasury App**: Payment intents, settlements, refunds, revenue accounting, cash drawer reconciliation
- **Notifications App**: Cash variance alerts, order ready notifications, integration failures, license reminders
- **Auth Service**: SSO and role claims for POS staff (cashiers, managers); tenant/outlet discovery

For more detail see `plan.md` and `docs/erd.md`.

## Current Status

- Architectural planning and documentation in progress.
- Track milestones via `CHANGELOG.md`.

