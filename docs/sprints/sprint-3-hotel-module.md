# Sprint 3: Hotel Module — pos-api

**Status:** ✅ Complete  
**Period:** March–April 2026  
**Last updated:** 2026-09-19  
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
Mirrors the existing table-reservation widget (`public/widget/table-booking.js`) but for a date-range stay instead of a single time slot:
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

**Deliberately not built (at the time)**: a full incremental daily-billing scheduler for `per_day_split`, and a formal evidence-approval SLA/escalation flow for damage reports — both would be new infrastructure/product decisions beyond closing the identified gaps. Both were built the following day; see the next section.

---

## Follow-up: Lost & Found, Live Booking-Logic Bugs, Hospitality Dashboard, Hotel BI (2026-09-19)

A live BOI Guest House booking session surfaced two data-correctness bugs the same day they happened, plus a set of product requests (repair-module gating, per-outlet dashboard, checkout auto-fill, hotel reporting depth). All closed same-day.

### 1. Incremental night billing + damage-report SLA reminders (previously deferred)
- New `RoomNightlyBillingScheduler` (`internal/platform/scheduler/room_nightly_billing.go`, hourly ticker): for every active `RoomGuest` whose outlet is on `per_day_split`, posts any night's `RoomFolioItem` that has come due (`elapsedDays+1`, capped at `guest.Nights`) but hasn't been posted yet. This is itemized posting, not payment collection — it does not attempt to charge a card/M-Pesa automatically for each night; front desk still collects via the normal folio flow.
- New `DamageReportReminderScheduler` (`internal/platform/scheduler/damage_report_reminder.go`, hourly ticker): flags a pending `RoomDamageReport` older than 24h with a one-time `hotel.damage.overdue` event (`metadata.overdue_notified` guards against re-firing), for notifications-service to act on. No formal escalation chain beyond this single notification.

### 2. Lost & Found (new)
New `LostFoundItem` entity (`internal/ent/schema/lostfounditem.go`, migration `20260919053428_add_lost_found_items.sql`): tracks an item found on the property through `stored` → `claimed`/`disposed`/`donated`, with category, location found/stored, optional guest attribution (auto-prefilled from `RoomGuest` when a `room_guest_id` is supplied), and photo evidence via the same local-media-volume convention as damage reports.
- Backend: `internal/http/handlers/hotel_lost_found.go` — create/list/get/claim/dispose, `POST .../lost-found/{id}/photo` upload.
- Frontend: `LostFoundModal` (log a found item, mirrors `DamageReportModal`), a `/hotel/lost-found` review page (status/category filters, claim with claimant name/notes, dispose/donate with a required reason), wired into the room detail sidebar and the hotel overview quick-links.

### 3. Nights-calculation bug (financial correctness)
Reported live: a 2-night booking billed 3 nights on the folio. Root cause was in TWO places, both fixed:
- **Frontend** (`hotel/rooms/[roomId]/page.tsx`): `handleCheckIn` recomputed nights from the raw millisecond gap between arrival/departure via `Math.ceil(...)`, silently overriding an explicit Nights value whenever the two picked times of day didn't align exactly to 24h multiples. Replaced with a calendar-day-based sync model (`calendarDaysBetween`, `departureFromNights`) where nights and the two datetime pickers stay in sync with exactly one "driver" at a time, applying the outlet's configured check-out time (see #5) rather than an arbitrary time-of-day.
- **Backend** (`internal/http/handlers/hotel_reports.go`'s `HotelOccupancyReport`): a SEPARATE instance of the same root-cause bug class — `occupied_room_nights` was computed from raw wall-clock hours since check-in divided by 24, not whole calendar nights. A guest who had just checked in (or a same-day check-in/checkout used for testing) contributed a near-zero fractional night even though a full night's charge had posted, so `ADR = room_revenue / occupied_room_nights` could read in the millions while occupancy showed 0.0%. Rewrote using calendar-day boundaries (`roomGuestNightInterval`, shared with the new trend endpoint below) — a guest still checked in as of the report window's end date now always counts as occupying that night in full. Two regression tests reproduce the exact reported scenario.

### 4. Room status / guest-visibility bug (data correctness)
Reported live: a just-booked room showed "Cleaning" on the rooms list, and its detail page said "This room is available" with no guest info. Two separate bugs:
- `GetRoom` only ever loaded the guest edge for `status=active` guests — a room mid-cleaning (guest already checked out) returned no guest at all, so the detail page couldn't show who had just stayed there. Now loads the single most recent guest regardless of status (`WithGuests` ordered by `checked_in_at desc`, limit 1); the frontend derives `lastGuest` (display) separately from `guest` (the narrower "currently active" view check-in/checkout actions still require).
- `UpdateHousekeepingTask` never touched `Room.Status` on completion — a checkout-clean/routine-clean/maintenance task marked "completed" never returned the room to `available`, so every checked-out room stayed permanently "stuck" in `cleaning` until a staff member manually patched its status. Now flips the room back to `available` when such a task completes, but ONLY if the room is currently `cleaning`/`maintenance` (never overrides `occupied`/`reserved`).
- Frontend room-detail page also gained a status-aware empty state (`ROOM_STATUS_COPY`), a "Last Guest" card, a `canCheckIn` gate matching the backend's own accepted-status set, and a manual "mark room available now" override for a room stuck in `cleaning`/`maintenance`.

### 5. Checkout auto-fill from booking policy
`BookingPolicy` gained `checkin_time`/`checkout_time` (HH:MM, default 14:00/10:00), editable from Settings → Booking Policy. The room detail check-in form now auto-fills the departure date from arrival + nights at the configured checkout time (and recomputes nights when the departure date is edited directly instead) — see #3 for the underlying date-sync fix this policy field feeds into.

### 6. Repair module: hospitality/services/quick_service no longer get it
Reverses a prior "available for every use case" decision: Repairs is now retail-only (`nav-config.ts`'s `hideForProfiles` + `use-module-access.ts`'s `USE_CASE_MODULES`), per current product direction — a hospitality or salon business doesn't do device repairs.

### 7. Per-outlet, per-use-case hospitality dashboard
New `HospitalityDashboard` component, rendered instead of the generic admin dashboard when `isHospitality`. Combines `useDashboardSummary` (POS order revenue) with the already-correct `useHotelOccupancyReport` client-side rather than modifying the shared `GetSummary` endpoint (which has other consumers and is deliberately left POS-order-only). Shows accommodation KPIs (room revenue, occupancy, ADR, RevPAR) unconditionally, and F&B/conference/facility widgets only when `hasModule`/`hasFeature` says the outlet actually offers them — a pure-accommodation property like BOI Guest House never sees an F&B revenue card it can't populate.

### 8. Hotel Reports: daily trend + room-type + booking-source BI
New `GET /reports/hotel-occupancy/trend` (same hospitality + hotel_module gate as the existing aggregate report), sharing `roomGuestNightInterval` with it so the two can never disagree. Returns:
- A daily bucket series (occupancy rate, ADR, room vs ancillary revenue) — charted on `/hotel/reports` as a combined stacked-revenue-bars + occupancy-line chart.
- Room-type performance (revenue + occupancy per `room_type`).
- Booking-source breakdown (`RoomGuest.source`: staff front-desk vs the online self-service widget vs API) — a field that existed since the original schema but was never surfaced in any report until now.

### 9. treasury-api: Room Revenue was invisible on the financial dashboard
Separate investigation of "how is hotel revenue channeled through to financial reports" found that while `PostPaymentToLedgerItemized` correctly posts room charges to account 4410 (Room Revenue, split from 4400 Sales Revenue), treasury-api's `businessRevenueAccountCodes()` — which feeds the Dashboard's headline Revenue KPI, the P&L Summary tab, and the Revenue by Outlet chart — hardcoded `{"4400", "4500"}` and was never updated to include 4410. Room revenue was visible on the Reports → Profit & Loss tab and the Chart of Accounts (both account-agnostic), but effectively invisible everywhere else. Fixed in `finance-service/treasury-api/internal/modules/finance/aggregates.go`.

**Deliberately not built**: a true incremental payment-collection mechanism beyond posting the itemized per-night charge (see #1); a formal SLA escalation chain beyond the single overdue notification (see #1); any change to the shared `GetSummary` endpoint (see #7).

### 10. Room status lifecycle: race conditions + a multi-task gap
A direct follow-up audit of the available→occupied→cleaning→available loop (prompted by a user question about exactly where each transition happens) found four real correctness gaps, all fixed:
- **CheckIn double-booking race**: the room's status was read once, validated in application code, and only flipped to `occupied` much later — after creating the `RoomGuest` and posting the folio charge — via an unconditional `UpdateOne`. Two concurrent check-ins for the same room (two front-desk terminals, or a retried double-submit) could both pass the early check before either committed, both create a guest, and both occupy the room. Fixed by making the final status flip a conditional bulk update (`WHERE status IN (available, reserved)`); zero affected rows now rolls back the whole transaction (guest + folio charge included) and returns 409.
- **CheckOut/SettleFolio/BatchCheckout double-checkout race**: same shape, on the way out — the active guest was found once, then flipped to `checked_out` unconditionally. Two concurrent checkout attempts (a double-click, or SettleFolio's auto-checkout racing a direct CheckOut call) could both fire the checkout side effects, including creating two `checkout_clean` housekeeping tasks. Fixed the same way (`WHERE status = active`) in all three call sites. BatchCheckout additionally stopped silently swallowing the update error — a failed status update used to still report the room as successfully checked out in the batch response, while the database left the guest active and the room occupied with no indication anything had gone wrong.
- **Detached-goroutine context-cancellation race**: `CheckOut` and `SettleFolio` both fire the auto-created `checkout_clean` housekeeping task from a `go func()` that outlives the handler, using `r.Context()` — which `net/http` cancels shortly after the handler returns. Under normal timing this genuinely race the response being written, silently dropping the task creation (leaving the room stuck in `cleaning` with no task for staff to ever complete — a different route to the same "stuck room" symptom the housekeeping-status-reset fix closed earlier in this session). Fixed with `context.WithoutCancel(r.Context())`.
- **Multiple-open-tasks-per-room gap**: `UpdateHousekeepingTask`'s "flip the room back to available" logic (added earlier this session) only checked the room's own current status, not whether any OTHER task for the same room was still pending/in_progress. A room with two open tasks (the auto-created `checkout_clean` plus a separately logged `maintenance` task, say) would flip to `available` the moment the FIRST one completed, silently leaving the second unresolved issue open against a room that now looked bookable. Fixed by checking for other open tasks before flipping.

Four new regression tests (`hotel_status_race_test.go`) fire concurrent goroutines at CheckIn/CheckOut and seed a multi-task room, each confirmed to fail without its corresponding fix.

**Found but deliberately NOT fixed (needs a product decision, not just a bug fix)**: `Room.status`'s `reserved` and `checkout` enum values are never set by any backend handler automatically — `reserved` only reachable via the unrestricted manual `PATCH /rooms/{id}/status`, meaning a confirmed `RoomBooking` never makes the room grid visually show "this room has an upcoming reservation." Fixing this needs real design input (how far ahead of arrival should a room show reserved; does an amendment/cancellation need to clear it; what happens with two future, non-overlapping bookings for the same room) rather than a unilateral guess. Similarly, `CreateHousekeepingTask` never takes a room out of service when a `maintenance` task is logged against a currently-bookable room — deliberately left alone since forcing every maintenance task to block bookings could be too aggressive without knowing the intended severity model.

### 11. Check-in 500'd on a missing phone, occupancy-based pricing, Settle Now, Edit Guest/Booking
Same-day follow-up, live-reported plus two explicit feature asks.

**Check-in validation gap (live bug)**: `RoomGuest.phone` is `NotEmpty` at the schema level, but only `id_number` had an explicit pre-check — an empty phone (nothing in the UI marked it required or blocked the submit) fell through to ent's raw validator error and 500'd as an opaque "failed to check in guest". Fixed: `guest_name` and `phone` now get the same clear-400 treatment `id_number` already had; the check-in form marks Phone required (asterisk, disabled-button guard, explicit client-side check).

**Occupancy-based pricing (new, researched against standard hotel PMS practice — Booking.com's child-policy model, Cloudbeds' base-rate-plus-extra-person matrix)**: a room rate can now optionally cover a base number of adults for free, with a per-night surcharge for each adult beyond that and each non-free child. Added to the existing `booking_policy` object (`OutletSetting.metadata`, no migration): `base_occupancy_adults`, `extra_adult_rate`, `child_free_under_age`, `extra_child_rate`. **Off by default** (`base_occupancy_adults <= 0` disables it entirely) — every tenant that hasn't configured this keeps charging the flat room rate regardless of adults/children, exactly as before this feature existed; BOI Guest House is unaffected unless it opts in. `occupancySurchargePerNight` (roombooking.go) computes the surcharge and is wired into `CheckIn`'s rate calculation; a child's age (from the existing but previously-uncollected `RoomGuest.child_ages` field) below the free-age threshold is always free regardless of count. Settings → Booking Policy gained an "Occupancy-based pricing" section; the check-in form gained a Child Ages input and shows a live estimated-total breakdown (base rate + extra adults + chargeable children) before Confirm, using the exact same formula server-side so the estimate never drifts from what's actually charged. Self-service/public booking's upfront quote is deliberately left as a rough average-rate estimate (matches how real hotels handle this too — extra-guest fees are typically confirmed at check-in, not shown upfront); the FINAL charge at check-in is fully covered by this fix regardless of booking source.

**Settle Now button (live bug — no way to take a payment without also checking out)**: `SettleFolio`/`CheckoutPanel` already fully supported this (a "check guest out when balance clears" checkbox, already user-togglable) — the gap was purely that the only button opening the panel was "Checkout & Settle Bill", implying checkout was mandatory. Added a second "Settle Now" button that opens the same panel with that checkbox defaulting to OFF, via a new `defaultCheckoutOnSettle` prop.

**Edit Guest / Booking (new)**: new `PATCH /{tenantID}/hotel/rooms/{id}/guest` (`UpdateGuest`, hotel_guest_edit.go) lets front desk correct an active guest's own contact/ID details, occupancy (adults/children/child_ages), and extend/shorten the stay (nights, recomputing `check_out_date` on calendar-day arithmetic — the same convention as everywhere else in this module). Deliberately a correction tool, not a re-billing one: it never touches `total_room_charge` or posts/adjusts any folio item, because doing that correctly needs to know which nights have already been folio-posted (varies by `payment_timing`) to avoid double- or under-billing — any resulting billing change goes through the existing "Add Folio Charge" action instead, same as every other ad-hoc charge. New `EditGuestModal` + "Edit Guest / Booking" button on the room detail page.

Six new regression tests total: `TestCheckIn_MissingPhone_ReturnsClearBadRequest`, `TestOccupancySurchargePerNight_MatchesStandardHotelPMSRule` (7 cases, pure unit test of the pricing arithmetic), `TestCheckIn_OccupancyPricing_UnconfiguredChargesFlatRateRegardlessOfHeadcount`, `TestUpdateGuest_EditsDetailsAndExtendsStay`.
