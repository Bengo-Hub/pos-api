# Sprint 9: Service Business Module — pos-api

**Status:** 🔴 Not Started  
**Period:** September–October 2026  
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
- [ ] `GET /{tenant}/pos/appointments` — list (filter: date, staff_id, status, outlet_id)
- [ ] `GET /{tenant}/pos/appointments/{id}` — detail with staff + service + client info
- [ ] `POST /{tenant}/pos/appointments` — create appointment (client, staff, service, start_time, duration_minutes)
- [ ] `PATCH /{tenant}/pos/appointments/{id}` — update (reschedule, reassign staff)
- [ ] `POST /{tenant}/pos/appointments/{id}/check-in` — mark client arrived
- [ ] `POST /{tenant}/pos/appointments/{id}/start` — mark service in progress
- [ ] `POST /{tenant}/pos/appointments/{id}/complete` — mark complete + create POS order
- [ ] `POST /{tenant}/pos/appointments/{id}/cancel` — cancel with reason
- [ ] `POST /{tenant}/pos/appointments/{id}/no-show` — mark no-show
- [ ] Validation: no double-booking for same staff at same time slot; resource conflicts checked

### Walk-In Service Queue
- [ ] `ServiceQueueEntry` schema — `id, tenant_id, outlet_id, client_name, client_phone (nullable), service_id (FK → CatalogItem), staff_id (nullable, assigned or auto-assigned), queue_position (int), status (waiting|in_progress|done|left), checked_in_at, started_at, completed_at, estimated_wait_minutes`
- [ ] `POST /{tenant}/pos/queue` — add walk-in to queue
- [ ] `GET /{tenant}/pos/queue` — list active queue (ordered by position)
- [ ] `POST /{tenant}/pos/queue/{id}/assign` — assign staff to entry
- [ ] `POST /{tenant}/pos/queue/{id}/start` — start service
- [ ] `POST /{tenant}/pos/queue/{id}/complete` — complete + create POS order
- [ ] `DELETE /{tenant}/pos/queue/{id}` — remove from queue (left/cancelled)
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
- [ ] `ServiceResource` schema — `id, tenant_id, outlet_id, name, resource_type (chair|bay|room|equipment), is_active`
- [ ] Appointment can be linked to a resource (e.g., car wash bay, styling chair)
- [ ] `GET /{tenant}/pos/resources` — list resources + current occupancy
- [ ] Resource conflict detection in appointment booking

### RBAC Additions
- [ ] New permissions: `pos.appointments.view`, `pos.appointments.change`, `pos.appointments.manage`
- [ ] New permissions: `pos.queue.view`, `pos.queue.change`
- [ ] New permissions: `pos.staff.view`, `pos.staff.manage`
- [ ] New permissions: `pos.commissions.view`, `pos.commissions.manage`, `pos.commissions.payout`
- [ ] New permissions: `pos.packages.view`, `pos.packages.change`, `pos.packages.manage`
- [ ] New permissions: `pos.clients.view`, `pos.clients.manage`
- [ ] New system roles: `stylist`, `therapist`, `technician` (service staff roles with limited RBAC)

### Migration
- [ ] Add `ServiceQueueEntry` ent schema
- [ ] Add `CommissionRule` ent schema
- [ ] Add `ServicePackage`, `ServicePackagePurchase`, `ServicePackageRedemption` ent schemas
- [ ] Add `ClientRecord` ent schema
- [ ] Add `ServiceResource` ent schema
- [ ] Wire `CommissionRecord` creation into `orders.Service.Complete()`
- [ ] Run `go generate ./internal/ent`
- [ ] Generate Atlas migration: `service_module`
- [ ] Update `docs/erd.md`

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
