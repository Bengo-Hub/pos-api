# Sprint 3: Hotel Module — pos-api

**Status:** 🔴 Not Started  
**Period:** May–June 2026  
**Goal:** Add hotel/lodge management — rooms, check-in/out, room folio charges, facilities booking

---

## Context

The `hotel-pos-v8.jsx` design prototype demonstrates a full hotel POS supporting:
- Room grid with 6 statuses (available, occupied, cleaning, maintenance, reserved, checkout)
- Check-in modal (guest name, phone, ID, nights calculation, auto room charge)
- Room folio (per-stay charge history: room, food, laundry, minibar, room service)
- Check-out (folio summary + settlement)
- Facilities (pool, gym, conference, spa, kids area) with booking management

**Role gating**: `rooms` and `facilities` tabs shown only to `receptionist` and `admin` roles.

---

## Ent Schemas to Add

### `room.go`
```
Room: id (UUID), tenant_id, outlet_id, room_number (string), name,
      room_type enum(standard|deluxe|suite|presidential|other),
      floor (int), rate_per_night (float64), currency (default KES),
      status enum(available|occupied|cleaning|maintenance|reserved|checkout),
      is_active (bool), metadata (JSON), created_at, updated_at
Edges: guests (→ RoomGuest), folio_items (→ RoomFolioItem)
Indexes: (tenant_id, outlet_id), (tenant_id, room_number) unique, (status)
```

### `roomguest.go`
```
RoomGuest: id (UUID), tenant_id, room_id (FK → Room),
           guest_name, phone, id_number (string),
           check_in_date (time.Time), nights (int),
           check_out_date (time.Time, computed or set),
           total_room_charge (float64),
           status enum(active|checked_out),
           checked_in_by (UUID, user_id ref),
           checked_out_by (UUID, nullable),
           checked_in_at (time.Time), checked_out_at (nullable),
           metadata (JSON), created_at, updated_at
Edges: room (← Room), folio_items (→ RoomFolioItem)
Indexes: (tenant_id, room_id), (tenant_id, status)
```

### `roomfolioitem.go`
```
RoomFolioItem: id (UUID), tenant_id, room_id (FK → Room),
               room_guest_id (FK → RoomGuest),
               description (string), amount (float64), currency (default KES),
               charge_type enum(room_charge|food|laundry|minibar|room_service|other),
               pos_order_id (UUID, nullable — linked POS order if applicable),
               created_at (Immutable), created_by (UUID, user_id ref),
               metadata (JSON)
Edges: room (← Room), guest (← RoomGuest)
Indexes: (tenant_id, room_id), (tenant_id, room_guest_id)
```

### `facility.go`
```
Facility: id (UUID), tenant_id, outlet_id, name,
          facility_type enum(pool|gym|conference|spa|kids_area|other),
          capacity (int), rate_per_session (float64), currency (default KES),
          opening_time (string "HH:MM"), closing_time (string "HH:MM"),
          status enum(available|occupied|maintenance|closed),
          is_active (bool), metadata (JSON), created_at, updated_at
Edges: bookings (→ FacilityBooking)
Indexes: (tenant_id, outlet_id), (tenant_id, status)
```

### `facilitybooking.go`
```
FacilityBooking: id (UUID), facility_id (FK → Facility), tenant_id,
                 room_guest_id (UUID, nullable — hotel guest reference),
                 guest_name, phone,
                 session_date (time.Time), start_time, end_time (string "HH:MM"),
                 guests_count (int), amount (float64), currency (default KES),
                 status enum(confirmed|cancelled|completed),
                 booked_by (UUID, user_id ref),
                 notes (string, optional), metadata (JSON), created_at
Edges: facility (← Facility)
Indexes: (tenant_id, facility_id), (tenant_id, session_date, status)
```

---

## Schema Updates to Existing Entities

### `posorder.go` — add hotel context fields
```
room_id (UUID, nullable)       — room service orders linked to a room
room_guest_id (UUID, nullable) — room service linked to a guest stay
order_subtype enum(dine_in|takeaway|room_service|delivery|bar_tab) default dine_in
```

---

## HTTP Endpoints

### Rooms
| Method | Path | Description | Permission |
|--------|------|-------------|------------|
| GET | `/{tenant}/hotel/rooms` | List rooms (filter: status, floor, type) | `pos.hotel.view` |
| GET | `/{tenant}/hotel/rooms/{id}` | Room detail + current guest + folio | `pos.hotel.view` |
| POST | `/{tenant}/hotel/rooms` | Create room | `pos.hotel.manage` |
| PATCH | `/{tenant}/hotel/rooms/{id}/status` | Update room status | `pos.hotel.change` |
| POST | `/{tenant}/hotel/rooms/{id}/check-in` | Check-in guest (creates RoomGuest + RoomFolioItem for room charge) | `pos.hotel.change` |
| POST | `/{tenant}/hotel/rooms/{id}/check-out` | Check-out (compute folio total, settle) | `pos.hotel.change` |
| POST | `/{tenant}/hotel/rooms/{id}/folio` | Post charge to room folio | `pos.hotel.change` |
| GET | `/{tenant}/hotel/rooms/{id}/folio` | List folio items for current/last stay | `pos.hotel.view` |

### Facilities
| Method | Path | Description | Permission |
|--------|------|-------------|------------|
| GET | `/{tenant}/hotel/facilities` | List facilities | `pos.hotel.view` |
| GET | `/{tenant}/hotel/facilities/{id}` | Facility detail + bookings | `pos.hotel.view` |
| POST | `/{tenant}/hotel/facilities` | Create facility | `pos.hotel.manage` |
| POST | `/{tenant}/hotel/facilities/{id}/book` | Create booking | `pos.hotel.change` |
| PATCH | `/{tenant}/hotel/facilities/bookings/{id}` | Update booking status | `pos.hotel.change` |
| GET | `/{tenant}/hotel/facilities/bookings` | List all bookings (filter: date, status) | `pos.hotel.view` |

---

## RBAC Permissions to Seed
Add to seed script under the `hotel` module:
- `pos.hotel.view` — view rooms and facilities
- `pos.hotel.change` — check-in, check-out, post charges, manage bookings
- `pos.hotel.manage` — create/edit rooms and facilities

Assign to roles:
- `pos_admin`: all hotel permissions
- `store_manager`: all hotel permissions
- `receptionist` (new system role): `pos.hotel.view` + `pos.hotel.change`
- `cashier`: `pos.hotel.view` only (to see folio for payment)
- `waiter`: `pos.hotel.view` only (for room service orders)

---

## Events Published
- `pos.room.checked_in` — notify notifications-service
- `pos.room.checked_out` — notify notifications-service, trigger treasury for folio settlement
- `pos.facility.booked` — audit trail

---

## Events Consumed
- `treasury.payment.success` — mark room folio settled on check-out payment

---

## Migration Steps
```bash
cd pos-service/pos-api
go generate ./internal/ent
go run cmd/migrate/main.go hotel_module
```

---

## Tasks
- [ ] Create `internal/ent/schema/room.go`
- [ ] Create `internal/ent/schema/roomguest.go`
- [ ] Create `internal/ent/schema/roomfolioitem.go`
- [ ] Create `internal/ent/schema/facility.go`
- [ ] Create `internal/ent/schema/facilitybooking.go`
- [ ] Update `internal/ent/schema/posorder.go` — add hotel context fields
- [ ] Run `go generate ./internal/ent`
- [ ] Run Atlas migration: `go run cmd/migrate/main.go hotel_module`
- [ ] Create `internal/modules/hotel/` — service + repository (rooms, guests, folio, facilities, bookings)
- [ ] Create `internal/http/handlers/hotel_handler.go`
- [ ] Register hotel routes in `internal/http/router/router.go`
- [ ] Update seed script with `receptionist` role + hotel permissions
- [ ] Update `docs/erd.md` with hotel entities
- [ ] Update Swagger: `swag init`
- [ ] Build and fix all errors: `go build ./...`
- [ ] Push to staging, merge to main
