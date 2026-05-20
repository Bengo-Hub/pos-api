# Sprint 8: Pharmacy Module — pos-api

**Status:** ✅ Core Delivered — prescriptions, drug interaction checks, and dispense endpoint shipped; controlled substance register and age verification pending  
**Period:** August–September 2026  
**Last updated:** 2026-05-21  
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
- [x] `Prescription` schema (`internal/ent/schema/prescription.go`)
- [x] `PrescriptionLine` schema (`internal/ent/schema/prescriptionline.go`)
- [x] `POST /{tenant}/pos/pharmacy/prescriptions` — register prescription (`pharmacy.go` handler)
- [x] `GET /{tenant}/pos/pharmacy/prescriptions` — list prescriptions
- [x] `GET /{tenant}/pos/pharmacy/prescriptions/{id}` — detail
- [x] `POST /{tenant}/pos/pharmacy/prescriptions/{id}/dispense` — dispense prescription (marks filled)
- [x] Route group gated by `RequireUseCase("pharmacy")` middleware

### Drug Interaction Checks
- [x] `DrugInteractionCheck` schema (`internal/ent/schema/druginteractioncheck.go`)
- [x] `POST /{tenant}/pos/pharmacy/interaction-checks` — log drug interaction check

### Controlled Substance Register
- [ ] `ControlledSubstanceLog` schema — not implemented
- [ ] Controlled substance dispensing register endpoints — not implemented

### Age Verification
- [ ] `catalog_items.minimum_age` field — not confirmed in schema
- [ ] Age verification endpoint on order line — not implemented

### Lot/Batch Dispensing
- [x] `pos_order_lines.lot_number` (string nullable) — in `POSOrderLine` schema
- [ ] `pos_order_lines.expiry_date` and `partial_units` — not confirmed in schema
- [ ] `GET /{tenant}/pos/catalog/items/{id}/lots` — not implemented

### Prescription-Only Item Blocking
- [ ] `catalog_items.requires_prescription` field — not confirmed
- [ ] Cart-add validation for prescription items — not implemented

### Returns Restriction
- [ ] `catalog_items.is_returnable` field — not confirmed
- [ ] Return block logic on refund — not implemented

### RBAC Additions
- [ ] `pos.pharmacy.*` permissions and `pharmacist` role — not yet seeded

### Migration
- [x] `Prescription` + `PrescriptionLine` + `DrugInteractionCheck` ent schemas added
- [x] Atlas migrations generated
- [ ] `minimum_age`, `is_controlled`, `requires_prescription`, `is_returnable` fields on `catalog_items` — not confirmed
- [ ] `expiry_date`, `partial_units`, `age_verification_method` on `pos_order_lines` — not confirmed
- [ ] `docs/erd.md` updated — pending

## Completion Notes (2026-05-21)

Pharmacy handler (`pharmacy.go`) is implemented and routes are registered under `RequireUseCase("pharmacy")` middleware. Core prescription CRUD, dispense, and drug interaction check endpoints are working. Controlled substance register, age verification, lot/batch dispensing, and prescription-only blocking remain unimplemented.

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
