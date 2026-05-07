# Sprint 8: Pharmacy Module — pos-api

**Status:** 🔴 Not Started  
**Period:** August–September 2026  
**Goal:** Regulated retail features for pharmacies — prescriptions, controlled substances, age verification, lot/batch tracking

---

## Context

Pharmacies operate under regulatory requirements that generic retail does not:
- Prescription drugs require a valid prescription before dispensing; the prescription number must be logged
- Controlled substances (narcotics, psychotropics) require a special dispensing register with quantity limits
- Age-restricted products (alcohol, certain OTC medications) require age verification at checkout
- Medications come in batches/lots with expiry dates; FIFO dispensing is mandatory
- Partial pack dispensing: a patient may receive 10 of 30 tablets from a box
- Medicine returns are restricted or prohibited

Inventory lot/batch tracking is handled by the inventory-api. This sprint focuses on the POS-side regulatory layer.

---

## Deliverables

### Prescription Management
- [ ] `Prescription` schema — `id, tenant_id, outlet_id, prescription_number (string), prescriber_name, prescriber_license, patient_name, patient_phone, date_issued, date_filled (nullable), pos_order_id (FK nullable), items: [{drug_name, strength, quantity, unit, refills_remaining}], status (pending|filled|partially_filled|expired|cancelled), created_by, created_at`
- [ ] `POST /{tenant}/pos/prescriptions` — register a new prescription
- [ ] `GET /{tenant}/pos/prescriptions` — list (filter: status, date range, patient phone)
- [ ] `GET /{tenant}/pos/prescriptions/{id}` — detail
- [ ] `POST /{tenant}/pos/prescriptions/{id}/fill` — link to POS order (mark filled)
- [ ] Validate: prescription must not be expired, refills must remain
- [ ] On pos_order with prescription items: require prescription_id to be attached

### Controlled Substance Register
- [ ] `ControlledSubstanceLog` schema — `id, tenant_id, outlet_id, drug_name, strength, batch_number, quantity_dispensed, unit, prescription_id (FK), patient_name, patient_id_number, dispensed_by (user_id), dispensed_at, witness_user_id (nullable), notes`
- [ ] `POST /{tenant}/pos/controlled-substances/log` — create dispensing entry (requires `pos.pharmacy.controlled` permission)
- [ ] `GET /{tenant}/pos/controlled-substances/log` — dispensing register (date range filter)
- [ ] Auto-create entry when a flagged `catalog_item.is_controlled = true` item is sold

### Age Verification
- [ ] `catalog_items.minimum_age` (int, default 0) — minimum age in years; 0 means no restriction
- [ ] On order line addition: if item has `minimum_age > 0`, attach `age_verification_method` to line
- [ ] `age_verification_method` enum: `id_card | passport | driving_license | override_manager`
- [ ] `POST /{tenant}/pos/orders/{order_id}/lines/{line_id}/verify-age` — record verification method + verifier user ID
- [ ] Manager override requires `pos.age_override.manage` permission

### Lot/Batch Dispensing
- [ ] `pos_order_lines.lot_number` (string nullable) — batch/lot number dispensed
- [ ] `pos_order_lines.expiry_date` (date nullable) — expiry of the dispensed batch
- [ ] `pos_order_lines.partial_units` (int nullable) — for partial-pack dispensing (e.g., 10 of 30 tablets)
- [ ] `GET /{tenant}/pos/catalog/items/{id}/lots` — list available lots from inventory-api (proxied)
- [ ] On order finalize: publish `pos.dispensing.completed` NATS event with lot details for inventory-api backflush

### Prescription-Only Item Blocking
- [ ] `catalog_items.requires_prescription` (bool, default false)
- [ ] On cart add: if item requires prescription and no prescription is attached to order, block with `402 Unprocessable`
- [ ] Allow override with `pos.pharmacy.override` permission (logs override reason)

### Returns Restriction
- [ ] `catalog_items.is_returnable` (bool, default true)
- [ ] On POS refund: if any line item has `is_returnable = false`, block return and show reason
- [ ] Controlled substance lines: returns always blocked regardless of `is_returnable`

### RBAC Additions
- [ ] New permissions: `pos.pharmacy.view`, `pos.pharmacy.change`, `pos.pharmacy.manage`, `pos.pharmacy.controlled`, `pos.pharmacy.override`, `pos.age_override.manage`
- [ ] New system role: `pharmacist` — has `pos.pharmacy.*` permissions

### Migration
- [ ] Add `Prescription` ent schema
- [ ] Add `ControlledSubstanceLog` ent schema
- [ ] Add `minimum_age`, `is_controlled`, `requires_prescription`, `is_returnable` fields to `catalog_items`
- [ ] Add `lot_number`, `expiry_date`, `partial_units`, `age_verification_method` to `pos_order_lines`
- [ ] Add `prescription_id` edge to `pos_orders`
- [ ] Run `go generate ./internal/ent`
- [ ] Generate Atlas migration: `pharmacy_module`
- [ ] Update `docs/erd.md`

---

## Use Cases Covered

| Use Case | Requirement |
|----------|------------|
| Prescription drug dispensing | Log prescription, validate, mark filled |
| Controlled substance dispensing | Dual-person dispensing register |
| Age-restricted product sale | ID check logged per transaction |
| Lot/batch FIFO dispensing | Track which batch was sold, expiry date |
| Partial pack dispensing | Sell units from open pack |
| Non-returnable items | Block returns on medications |
