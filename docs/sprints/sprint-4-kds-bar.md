# Sprint 4: KDS & Bar Display — pos-api

**Status:** 🟡 Partial (entities exist, HTTP endpoints missing)  
**Period:** May 2026  
**Goal:** Kitchen Display System and Bar Display REST endpoints — ticket creation, item-level status, station routing, call-waiter

---

## Context

The `hotel-pos-v8.jsx` design shows KDS with:
- Separate kitchen and bar queues
- Order cards with table/waiter/guest count
- Item-level status per ticket (pending → cooking → ready)
- Timer showing order age (green → orange after 10 min → red after 15 min)
- "Start All", "Done", "Call Waiter" actions

**Existing**: `KDSStation` and `KDSTicket` ent schemas are already defined (March 2026).  
**Missing**: HTTP endpoints to query tickets by station, update ticket/item status, call waiter.

---

## Existing Entities (Already in Schema)

### `KDSStation`
- id, tenant_id, outlet_id, name, category_filter (JSON array of category codes), sort_order, is_active

### `KDSTicket`
- id, tenant_id, station_id (FK → KDSStation), order_id, order_number
- status: `pending | in_progress | ready | served | voided`
- items (JSON array: `[{line_id, sku, name, qty, kds_status}]`)
- received_at, started_at, completed_at, priority

---

## KDS Ticket Creation (Wire to Order Flow)

When `POST /{tenant}/pos/orders/{id}/status` transitions order to `open`:
1. For each order line, determine destination station (via `catalog_item.kds_station` or `category → station` mapping from `KDSStation.category_filter`)
2. Group lines by station
3. Create one `KDSTicket` per station with the grouped line items

---

## HTTP Endpoints to Add

### KDS Queries
| Method | Path | Description | Permission |
|--------|------|-------------|------------|
| GET | `/{tenant}/pos/kds/stations` | List KDS stations for outlet | `pos.kds.view` |
| GET | `/{tenant}/pos/kds/kitchen` | Kitchen queue (pending/in_progress/ready tickets) | `pos.kds.view` |
| GET | `/{tenant}/pos/kds/bar` | Bar queue (pending/in_progress/ready tickets) | `pos.kds.view` |
| GET | `/{tenant}/pos/kds/tickets` | All tickets (filter: station_id, status, since) | `pos.kds.view` |

### KDS Actions
| Method | Path | Description | Permission |
|--------|------|-------------|------------|
| POST | `/{tenant}/pos/kds/tickets/{id}/start` | Mark ticket in_progress (start cooking) | `pos.kds.change` |
| POST | `/{tenant}/pos/kds/tickets/{id}/ready` | Mark ticket ready (food plated) | `pos.kds.change` |
| POST | `/{tenant}/pos/kds/tickets/{id}/serve` | Mark ticket served (waiter collected) | `pos.kds.change` |
| POST | `/{tenant}/pos/kds/tickets/{id}/void` | Void ticket (order cancelled) | `pos.kds.manage` |
| POST | `/{tenant}/pos/kds/tickets/{id}/call-waiter` | Trigger waiter notification (push/NATS) | `pos.kds.change` |

### KDS Station Management
| Method | Path | Description | Permission |
|--------|------|-------------|------------|
| POST | `/{tenant}/pos/kds/stations` | Create station | `pos.kds.manage` |
| PUT | `/{tenant}/pos/kds/stations/{id}` | Update station (name, category_filter) | `pos.kds.manage` |

---

## RBAC Permissions to Seed
Add to seed under `kds` module:
- `pos.kds.view` — view kitchen/bar queues
- `pos.kds.change` — start, ready, serve, call-waiter actions
- `pos.kds.manage` — void, station management

Assign to roles:
- `pos_admin`: all kds permissions
- `store_manager`: all kds permissions
- `cashier`: `pos.kds.view` only
- `waiter`: `pos.kds.view` + `pos.kds.change` (can mark as served)
- `kitchen` (new system role): `pos.kds.view` + `pos.kds.change`
- `bar` (new system role): `pos.kds.view` + `pos.kds.change`

---

## Events Published
- `pos.kds.ticket.ready` — notify waiter that order is ready (via notifications-service)
- `pos.kds.waiter.called` — alert waiter to collect food

---

## Polling vs WebSocket
- MVP: polling every 5 seconds via TanStack Query `refetchInterval: 5000`
- Post-MVP: WebSocket/SSE for real-time KDS updates

---

## Tasks
- [ ] Create `internal/modules/kds/` — service + repository
- [ ] Create `internal/http/handlers/kds_handler.go`
- [ ] Wire ticket creation into order status transition (`orders.Service`)
- [ ] Register KDS routes in `internal/http/router/router.go`
- [ ] Update seed script with `kitchen` and `bar` system roles + KDS permissions
- [ ] Update `docs/erd.md` to document KDS endpoints
- [ ] Update Swagger: `swag init`
- [ ] Build and fix all errors: `go build ./...`
- [ ] Push to staging, merge to main
