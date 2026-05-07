# POS API — Implementation Plan

**Last updated:** 2026-05-07  
**Module:** `github.com/bengobox/pos-service` (Go 1.24)  
**Database:** PostgreSQL 17 (local: `C:\Program Files\PostgreSQL\17`)  
**Migrations:** Ent + Atlas versioned migrations (`cmd/migrate/main.go`)

---

## Current State (May 2026)

### Completed
- **Sprint 1 (✅):** Auth, RBAC (126 perms, 5 system roles), devices, device sessions, outlets, licensing
- **Sprint 2 (✅):** Catalog, orders, payments, tables, cash drawer, bar tabs, promotions, gift cards, inventory touchpoint entities, outbox events

### In Progress / Planned
- **Sprint 3 (🔴):** Hotel module — rooms, guests, folio, facilities, bookings
- **Sprint 4 (🟡):** KDS endpoints — entities exist, HTTP handlers missing
- **Sprint 5 (🟡):** ERP gaps — daily closing, returns, receipt PDF
- **Sprint 6 (🟡):** Inventory + treasury wiring — events defined, S2S calls not wired

---

## Implementation Priorities

### Priority 1: Hotel Module (Sprint 3)

**New Ent schemas** (`internal/ent/schema/`):
- `room.go` — Room entity
- `roomguest.go` — RoomGuest entity
- `roomfolioitem.go` — RoomFolioItem entity
- `facility.go` — Facility entity
- `facilitybooking.go` — FacilityBooking entity

**Schema updates:**
- `posorder.go` — add `room_id` (UUID nillable), `room_guest_id` (UUID nillable), `order_subtype` enum

**Migration:**
```powershell
cd pos-service/pos-api
go generate ./internal/ent
go run cmd/migrate/main.go hotel_module
```

**New module:** `internal/modules/hotel/` (service + repository)  
**New handler:** `internal/http/handlers/hotel_handler.go`  
**Router registration:** `internal/http/router/router.go`

**New RBAC:** `pos.hotel.view`, `pos.hotel.change`, `pos.hotel.manage`  
**New role:** `receptionist`

---

### Priority 2: KDS Endpoints (Sprint 4)

Schemas already exist (`kdsstation.go`, `kdsticket.go`).

**New module:** `internal/modules/kds/` (service + repository)  
**New handler:** `internal/http/handlers/kds_handler.go`  
**Wire ticket creation** into `orders.Service` on status transition `→ open`

**New RBAC:** `pos.kds.view`, `pos.kds.change`, `pos.kds.manage`  
**New roles:** `kitchen`, `bar`

---

### Priority 3: Treasury + Inventory Wiring (Sprint 6)

**New clients:**
- `internal/modules/inventory/client.go` — S2S inventory client
- `internal/modules/treasury/client.go` — S2S treasury client

**Wire points:**
- `orders.Service.Complete()` → call inventory consumption
- `payments.Service.Record()` → call treasury intent for card/mpesa
- `internal/platform/events/subscribers.go` → add NATS subscribers

**New env vars:**
```
INVENTORY_SERVICE_URL
INVENTORY_SERVICE_API_KEY
TREASURY_SERVICE_URL
TREASURY_SERVICE_API_KEY
NOTIFICATIONS_SERVICE_URL
NOTIFICATIONS_SERVICE_API_KEY
```

---

## Architecture Patterns

### Module Structure

```
internal/modules/{module}/
  repository.go    — Ent queries (no business logic)
  service.go       — Business logic (calls repository + external clients)
  types.go         — Request/response DTOs
```

### S2S Client Pattern

```go
// internal/modules/{service}/client.go
type Client struct {
    httpClient *http.Client
    baseURL    string
    apiKey     string
}

func New(baseURL, apiKey string) *Client { ... }
func (c *Client) DoSomething(ctx context.Context, ...) error {
    // shared-service-client handles retry + auth header injection
}
```

### Handler Pattern

```go
// internal/http/handlers/{module}_handler.go
type HotelHandler struct {
    svc    *hotel.Service
    logger *zap.Logger
}

func (h *HotelHandler) GetRooms(c *fiber.Ctx) error {
    // 1. Extract tenantSlug from path param
    // 2. Call svc method
    // 3. Return JSON response
}
```

### Router Registration

```go
// internal/http/router/router.go
hotel := v1.Group("/:tenant/hotel", authMiddleware, rbacMiddleware)
hotel.Get("/rooms", hotelHandler.GetRooms)
hotel.Get("/rooms/:id", hotelHandler.GetRoom)
hotel.Post("/rooms/:id/check-in", hotelHandler.CheckIn)
hotel.Post("/rooms/:id/check-out", hotelHandler.CheckOut)
// ...
```

---

## Migration Workflow

```powershell
# 1. Update schema file(s) in internal/ent/schema/
# 2. Regenerate Ent code
cd pos-service/pos-api
go generate ./internal/ent

# 3. Generate Atlas migration (uses local PostgreSQL 17)
go run cmd/migrate/main.go <migration_name>

# 4. Verify migration SQL in internal/ent/migrate/migrations/
# 5. Verify atlas.sum updated
# 6. Build and test
go build ./...
```

---

## Key Files Reference

| File | Purpose |
|------|---------|
| `internal/ent/schema/` | All Ent entity schemas |
| `internal/ent/migrate/migrations/` | Atlas versioned SQL migrations |
| `internal/http/router/router.go` | Route registration |
| `internal/http/handlers/` | HTTP handlers (one file per module) |
| `internal/modules/` | Business logic services |
| `internal/platform/events/` | NATS publisher + subscribers |
| `cmd/migrate/main.go` | Migration runner |
| `cmd/seed/main.go` | Seed data (roles, permissions, tenders, catalog) |
| `docs/erd.md` | Entity documentation |
| `docs/integrations.md` | Service-to-service integration docs |
| `docs/sprints/` | Sprint-by-sprint task tracking |

---

## Build & Deploy

```powershell
# Build
cd pos-service/pos-api
go build ./...

# Swagger
swag init

# Run locally
go run cmd/api/main.go

# Deploy (via devops-k8s CI)
# Edit apps/pos-service/values.yaml for env vars
# Image tags set by build.sh — never edit manually
```
