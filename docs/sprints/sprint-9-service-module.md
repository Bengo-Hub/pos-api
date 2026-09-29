# Sprint 9: Service Business Module — pos-api

**Status:** ✅ Core Delivered — appointments (full CRUD + all status actions), staff schedules, commissions, walk-in queue, and resource management shipped; packages, client records CRUD, and commission rules/payout pending  
**Period:** September–October 2026  
**Last updated:** 2026-05-25  
**Goal:** Appointments, staff commissions, service queues, and packages for barbershop/salon, clinic, car wash, and service-based businesses

---

## Context

Service businesses differ from retail and hospitality:
- Revenue is time-based, not product-based — a service occupies a staff member and/or a resource (chair, bay, room) for a defined duration
- Appointments are the primary scheduling unit; walk-ins join a service queue
- Staff earn commissions on services they perform — commission structures vary (flat, percentage, tiered)
- Packages and bundles allow pre-purchase of multiple sessions at a discount
- A "sale" may span multiple visits (e.g., a course of treatments)
- Clinics require patient records and visit notes; salons require client preference cards

The `Appointment`, `StaffMember`, and `CommissionRecord` schemas already exist in the codebase. This sprint completes the HTTP layer and adds package/bundle and service queue management.

---

## Deliverables

### Appointment Management (complete HTTP layer)
- [x] `GET /{tenant}/pos/appointments` — list (filter: date, staff_id, status, outlet_id)
- [x] `GET /{tenant}/pos/appointments/{id}` — detail with staff + service + client info
- [x] `POST /{tenant}/pos/appointments` — create appointment (client, staff, service, start_time, duration_minutes, deposit)
- [x] `PUT /{tenant}/pos/appointments/{id}` — update (reschedule, reassign staff)
- [x] `GET /{tenant}/pos/appointments/availability` — available slots for a staff/date
- [x] `POST /{tenant}/pos/appointments/{id}/check-in` — mark client arrived (status → confirmed)
- [x] `POST /{tenant}/pos/appointments/{id}/start` — mark service in progress (status → in_progress)
- [x] `POST /{tenant}/pos/appointments/{id}/complete` — mark complete (status → completed)
- [x] `POST /{tenant}/pos/appointments/{id}/cancel` — cancel (status → cancelled)
- [x] `POST /{tenant}/pos/appointments/{id}/no-show` — mark no-show (status → no_show)
- [ ] Validation: no double-booking for same staff at same time slot; resource conflicts checked

### Walk-In Service Queue
- [x] `ServiceQueueEntry` schema (`internal/ent/schema/servicequeueentry.go`)
- [x] `POST /{tenant}/pos/queue/entries` — add walk-in to queue
- [x] `GET /{tenant}/pos/queue` — list active queue (ordered by position)
- [x] `PATCH /{tenant}/pos/queue/entries/{entryID}/status` — update status (waiting → in_progress → done/left)
- [ ] `POST /{tenant}/pos/queue/{id}/assign` — explicit staff assignment endpoint
- [ ] Auto-estimate wait time based on active service durations ahead in queue

### Staff Member Management (complete HTTP layer)
- [ ] `GET /{tenant}/pos/staff` — list staff (filter: outlet_id, is_active, role)
- [ ] `GET /{tenant}/pos/staff/{id}` — detail with services, schedule, commission summary
- [ ] `POST /{tenant}/pos/staff` — create staff member
- [ ] `PATCH /{tenant}/pos/staff/{id}` — update (name, phone, role, is_active)
- [ ] `GET /{tenant}/pos/staff/{id}/schedule` — available slots for a given date
- [ ] `GET /{tenant}/pos/staff/{id}/appointments` — appointments for date range

### Commission Management (complete HTTP layer)
- [ ] `CommissionRule` schema — `id, tenant_id, staff_id (FK, nullable — null = applies to all staff), catalog_item_id (FK, nullable — null = applies to all services), rule_type enum(flat|percentage|tiered), flat_amount (nullable), percentage (nullable), tier_rules (JSON: [{min_sales, max_sales, rate}]), effective_from, effective_to (nullable)`
- [ ] `GET /{tenant}/pos/commissions/rules` — list commission rules
- [ ] `POST /{tenant}/pos/commissions/rules` — create rule
- [ ] `PATCH /{tenant}/pos/commissions/rules/{id}` — update rule
- [ ] Wire commission calculation into order completion: on `pos.sale.finalized` event, compute commissions per staff per service line and create `CommissionRecord` entries
- [ ] `GET /{tenant}/pos/commissions` — list records (filter: staff_id, date range, status: pending|paid)
- [ ] `GET /{tenant}/pos/staff/{id}/commissions` — commission summary for a staff member
- [ ] `POST /{tenant}/pos/commissions/payout` — mark commissions as paid (batch by staff + date range)

### Service Packages & Bundles
- [ ] `ServicePackage` schema — `id, tenant_id, outlet_id, name, description, price, currency, sessions_total (int), validity_days (int), applicable_services (JSON: [catalog_item_id]), is_active, created_at`
- [ ] `ServicePackageRedemption` schema — `id, package_purchase_id (FK), tenant_id, pos_order_id (FK), redeemed_by (staff_id), redeemed_at, service_id (FK)`
- [ ] `ServicePackagePurchase` schema — `id, tenant_id, package_id (FK), client_name, client_phone, pos_order_id (FK — initial purchase), sessions_used (int), sessions_remaining (int), expires_at, status (active|exhausted|expired|cancelled)`
- [ ] `POST /{tenant}/pos/packages` — create package definition
- [ ] `GET /{tenant}/pos/packages` — list packages
- [ ] `POST /{tenant}/pos/packages/{id}/sell` — sell package to client (creates POS order)
- [ ] `GET /{tenant}/pos/packages/purchases?phone={client_phone}` — look up client's purchased packages
- [ ] `POST /{tenant}/pos/packages/purchases/{id}/redeem` — redeem one session from package

### Client Records (Salon/Clinic)
- [ ] `ClientRecord` schema — `id, tenant_id, outlet_id, full_name, phone (unique per tenant), email (nullable), date_of_birth (nullable), gender (nullable), notes (text), preferences (JSON), created_at, updated_at`
- [ ] `POST /{tenant}/pos/clients` — create or upsert by phone
- [ ] `GET /{tenant}/pos/clients?phone={phone}` — lookup client
- [ ] `GET /{tenant}/pos/clients/{id}` — detail with appointment history + package balances
- [ ] `PATCH /{tenant}/pos/clients/{id}` — update client notes/preferences
- [ ] Auto-link appointments and orders to client via phone number

### Resource / Bay Management
- [x] `Resource` schema (`internal/ent/schema/resource.go`) — `id, tenant_id, outlet_id, name, type (chair|room|table|equipment|general), status (available|occupied|maintenance|reserved), notes, timestamps`
- [x] `GET /{tenant}/pos/resources` — list resources (filter: type, status); gated by `RequireUseCase("services")`
- [x] `POST /{tenant}/pos/resources` — create resource
- [x] `PATCH /{tenant}/pos/resources/{id}` — update status/notes
- [x] Atlas migration generated: `20260525004518_add_resources_and_pharmacy_patients.sql`
- [ ] Appointment can be linked to a resource (resource conflict detection in booking)

### RBAC Additions
- [ ] New permissions: `pos.appointments.view`, `pos.appointments.change`, `pos.appointments.manage`
- [ ] New permissions: `pos.queue.view`, `pos.queue.change`
- [ ] New permissions: `pos.staff.view`, `pos.staff.manage`
- [ ] New permissions: `pos.commissions.view`, `pos.commissions.manage`, `pos.commissions.payout`
- [ ] New permissions: `pos.packages.view`, `pos.packages.change`, `pos.packages.manage`
- [ ] New permissions: `pos.clients.view`, `pos.clients.manage`
- [ ] New system roles: `stylist`, `therapist`, `technician` (service staff roles with limited RBAC)

### Migration
- [x] `ServiceQueueEntry` ent schema — shipped in prior sprint
- [x] `Resource` ent schema — `20260525004518_add_resources_and_pharmacy_patients.sql`
- [x] `go generate ./internal/ent` — all ent files generated
- [ ] `CommissionRule` ent schema
- [ ] `ServicePackage`, `ServicePackagePurchase`, `ServicePackageRedemption` ent schemas
- [ ] `ClientRecord` ent schema
- [ ] Wire `CommissionRecord` creation into `orders.Service.Complete()`
- [ ] Update `docs/erd.md`

### Service profiles and job orders (2026-09-29)

Full design: [../service-profiles.md](../service-profiles.md).

- [x] Service profile registry (10 trades) with `GET /pos/service-profiles` and `PATCH /pos/settings/service-profile`
- [x] Catalog narrows a services outlet's SERVICE items to its trade (inventory item use case)
- [x] `service_job` order subtype: job header in order metadata, per-line spec sheets, production tickets
- [x] `PATCH /pos/orders/{id}/job` (stages, proof, brief, attachments, collected) with audit events
- [x] `POST /pos/orders/job-attachments` (512 KB images or PDF), shared `storeTenantMedia` helper
- [x] Production board on the KDS routes for services outlets; final stage hands the job to payment
- [x] `GET /pos/jobs/summary` backed by the `posorder_service_jobs` partial index
- [x] Printing and branding shipped end to end and deployed (pos-api `6b24fac`, pos-ui `23a4697`)
- [ ] Appointment profiles: per-line performer, commission attribution, style reference media (phase 3)
- [ ] Garage, laundry, tailoring, car wash polish (phase 3)
- [ ] Plan feature codes `service_jobs`, `appointments`, `online_booking` (phase 4)

## Completion Notes (2026-05-25)

Full appointment CRUD and all status action endpoints are shipped. Walk-in queue (list/create/patch status) is implemented under `RequireUseCase("services")`. Resource management (list/create/patch status) is implemented with `Resource` ent schema and Atlas migration. Commission records (list/get) are shipped. Service packages, client records CRUD, commission rules/payout, and appointment-to-resource conflict detection remain unimplemented.

Implemented route coverage:
- `GET /appointments`, `POST /appointments`, `GET /appointments/availability`, `GET /appointments/{id}`, `PUT /appointments/{id}` — all under `RequireUseCase("services")`
- `POST /appointments/{id}/check-in`, `POST /appointments/{id}/start`, `POST /appointments/{id}/complete`, `POST /appointments/{id}/cancel`, `POST /appointments/{id}/no-show`
- `GET /queue`, `POST /queue/entries`, `PATCH /queue/entries/{entryID}/status` — under `RequireUseCase("services")`
- `GET /resources`, `POST /resources`, `PATCH /resources/{resourceID}` — under `RequireUseCase("services")`
- `GET /staff/{staffID}/schedule`, `PUT /staff/{staffID}/schedule`
- `GET /commissions`, `GET /commissions/{id}`

---

## Use Cases Covered

| Use Case | Business Types |
|----------|---------------|
| Appointment booking + reschedule | Salon, clinic, spa, physiotherapy |
| Walk-in queue with wait-time estimate | Barbershop, car wash, clinic |
| Service staff commission calculation | Salon, barbershop, spa |
| Service package / session bundle | Gym, spa, physiotherapy, beauty |
| Client preference cards | Salon, clinic |
| Resource/bay scheduling | Car wash, dental clinic, massage spa |
| Staff availability calendar | All service businesses |
