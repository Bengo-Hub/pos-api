# Sprint 3: Hotel Module — pos-api

**Status:** ✅ Complete  
**Period:** March–April 2026  
**Last updated:** 2026-05-09  
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
- [x] Create `internal/ent/schema/room.go`
- [x] Create `internal/ent/schema/roomguest.go`
- [x] Create `internal/ent/schema/roomfolioitem.go`
- [x] Create `internal/ent/schema/facility.go`
- [x] Create `internal/ent/schema/facilitybooking.go`
- [x] Update `internal/ent/schema/posorder.go` — add hotel context fields
- [x] Run `go generate ./internal/ent`
- [x] Run Atlas migration: `go run cmd/migrate/main.go hotel_module`
- [x] Create `internal/modules/hotel/` — service + repository (rooms, guests, folio, facilities, bookings)
- [x] Create `internal/http/handlers/hotel_handler.go`
- [x] Register hotel routes in `internal/http/router/router.go`
- [x] Update seed script with `receptionist` role + hotel permissions
- [x] Update `docs/erd.md` with hotel entities
- [x] Update Swagger: `swag init`
- [x] Build and fix all errors: `go build ./...`
- [x] Push to staging, merge to main

## Completion Notes (2026-05-09)

Audit confirmed all Ent schemas exist: `room.go`, `roomguest.go`, `roomfolioitem.go`, `facility.go`, `facilitybooking.go`. HTTP handler `hotel_handler.go` is in place. Endpoints operational under `/{tenant}/hotel/rooms` (GET/POST/PATCH) and `/{tenant}/hotel/facilities` (GET/POST). Full check-in, check-out, folio, and facilities booking flows are wired.

---

## Follow-up: Self-Service Booking, Damage Reports, Payment-Timing Enforcement (2026-09-18)

A later audit of the live module (see `.claude/memory/boi-guest-house-hotel-audit-and-room-load-2026-09-18.md`) found three real gaps against this original spec, all closed the same day.

### 1. Room payment-timing policy now actually enforced
`OutletSetting.metadata.booking_policy.payment_timing` (`settle_at_checkout` | `pay_upfront` | `per_day_split`, edited from Settings → Booking Policy) existed as a stored setting since the booking-policy feature shipped, but `CheckIn` never read it. Now:
- `pay_upfront` requires an immediate desk tender (cash/card_manual/mpesa) supplied with the check-in call; the amount is posted via a new shared `recordFolioPayment` helper (extracted from `SettleFolio`'s treasury-intent logic — `SettleFolio` now calls the same helper, no behavior change there).
- `per_day_split` posts the room charge as one `RoomFolioItem` per night instead of one lump sum. No scheduler exists to defer collection day-by-day — this gives per-night folio itemization, not incremental daily billing.
- A payment-recording failure at check-in doesn't roll back the check-in itself (the guest already occupies the room); it's surfaced via a `payment_recorded` response field so the desk can collect it via the normal Settle flow instead, which still gates checkout on balance regardless.

### 2. Self-service room booking (guest-facing widget)
Mirrors the existing table-reservation widget (`public/widget/booking.js`) but for a date-range stay instead of a single time slot:
- `RoomBooking.status` enum gained `pending` (plain varchar column, no DB constraint — no migration needed for this part).
- New public (unauthenticated) endpoints, alongside `/pos/reservations` in the router's `pub` group:
  - `GET /{tenant}/pos/room-bookings/availability?outlet_id=&arrival_date=&departure_date=` — per room_type available-room count + average rate, for the requested date range (room overlap computed the same way `HotelOccupancyReport` computes occupied room-nights).
  - `POST /{tenant}/pos/room-bookings` — creates a `RoomBooking{source:online, status:pending}` after re-validating availability server-side; no separate confirm endpoint was needed since the existing `UpdateRoomBooking` status transition already handles it.
  - `GET /{tenant}/pos/room-bookings/policy?outlet_id=` — the guest-facing cancellation/payment terms (reuses `resolveBookingPolicy`), so the widget's confirmation screen always reflects the real configured policy instead of static text.
- New widget: `pos-ui/public/widget/room-booking.js`.
- pos-ui's Bookings page gained a `pending` status filter/badge, and both amend and cancel now show the policy-computed fee **before** the staff confirms (client-side preview mirroring `computeBookingFee`), not just in the after-the-fact success toast.

### 3. Damage/fine report workflow
Previously a "damage" charge was just a folio line with no review step. New `RoomDamageReport` entity (`pending` → `approved`/`rejected`) with a real migration (`20260918200628_add_room_damage_reports.sql`, brand-new table):
- Front desk/housekeeping logs one via `POST /{tenant}/hotel/rooms/{id}/damage-reports` (`pos.hotel.change`), with optional photo evidence uploaded through `POST /{tenant}/hotel/damage-evidence/upload` — reuses pos-api's existing local media-volume convention (`internal/http/handlers/media.go`'s screensaver uploader pattern, same `MEDIA_ROOT`), not a new storage mechanism.
- A manager approves (`pos.hotel.manage`) — if the reported stay is still active, this posts the amount to the guest's folio as `charge_type=damage` through the same creation path `PostFolioCharge` uses, and links the resulting `RoomFolioItem`; if the guest already checked out, the report is still marked approved but nothing is auto-posted (`folio_posted:false` in the response) — or rejects with a required reason.
- New pos-ui page `/hotel/damage-reports` (list + approve/reject) and a "Report Damage" action on the room detail page, available regardless of occupancy.
- Also fixed the same day: pos-ui's "Add folio charge" dropdown was missing `damage` as a selectable option (had an invalid `"service"` value instead — silently fell back to `other` server-side).
- `BatchCheckout` (group/tour checkout) now applies the same outstanding-balance gate as single-room `CheckOut` — it previously force-checked-out every room regardless of unpaid balance.

**Deliberately not built**: a full incremental daily-billing scheduler for `per_day_split`, and a formal evidence-approval SLA/escalation flow for damage reports — both would be new infrastructure/product decisions beyond closing the identified gaps.
