# POS — Sprint: Repair / Job-Card Module (Retail POS Revamp)

**Created:** 2026-06-07 · **Driver:** `/.claude/plans/_audit-parts/retail-pos-audit-and-roadmap-2026-06-07.md`
**Status:** Planned (Phase 5) · **Owner:** pos-api **services module** (decided — not ticketing-service).

> godigital ships a Repair module (device intake → diagnosis → parts → settle). Codevertex had no
> SoT; it fits the existing pos-api services/appointments module: counter intake, parts drawn from
> inventory, settled via POS payment (treasury). Reuse `StaffMember`, `Appointment`, `CommissionRecord`.

## Schemas (Ent + Atlas)
- **`RepairJob`**: tenant_id, outlet_id, job_number (seq), `crm_contact_id` (customer), device/
  item description, serial/imei, reported_fault, status (received→diagnosing→awaiting_parts→
  in_repair→ready→collected→cancelled), assigned_staff_id, estimate_amount, deposit_amount,
  diagnosis_notes, created_by, timestamps.
- **`RepairJobPart`**: repair_job_id, `inventory_item_id` (part), qty, unit_cost, billed flag —
  consumes stock via existing `pos.sale.finalized`/consumption path on settle.
- **`RepairJobEvent`**: status-change audit trail (who/when/note).

## Endpoints
- `POST/GET/PUT /{tenant}/repairs` (+ `/{id}`, `/{id}/status`, `/{id}/parts`, `/{id}/settle`).
- Settle → create POS order for labour + parts → tender via treasury; deposit handled as advance
  (`wallet`) or partial payment; warranty flag.

## Events
- `pos.repair.received`, `pos.repair.ready`, `pos.repair.collected` → notifications (SMS to customer).

## Definition of Done
- [ ] `go build ./...`; Ent+Atlas migration; RBAC perms (`pos.repair.*`); subscription-gated add-on.
- [ ] E2E: intake → add parts (inventory consumption) → settle (treasury) → collected + SMS.
