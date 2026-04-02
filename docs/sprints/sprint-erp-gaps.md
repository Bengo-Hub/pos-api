# Sprint: ERP E-commerce Gaps — pos-api

**Created:** April 2026
**Status:** Planning
**Goal:** Close feature gaps identified from ERP ecommerce/POS module audit before ERP module deletion (Phase 1)

---

## Context

The ERP `ecommerce/pos/` module contains daily closing reconciliation, sale return/exchange workflows, and receipt PDF generation that may not yet be fully covered by pos-api. These must be verified and implemented before the ERP module can be removed.

---

## Gap 1: Daily Closing Reconciliation Report

**ERP source:** `ecommerce/pos/` — `DailyClosing` model
**Priority:** P1
**Status:** Pending — verify ShiftSummary coverage

### Current State

pos-api has `ShiftSummary` which tracks shift-level totals (cash, card, mobile money) and cash drawer reconciliation. The ERP `DailyClosing` model aggregates across all shifts for a given outlet+date, producing a single end-of-day report.

### Required

- [ ] **POS-ERP-01:** Audit `ShiftSummary` schema — confirm it captures:
  - Total sales by payment method (cash, card, M-Pesa, split)
  - Total refunds/returns
  - Expected vs actual cash in drawer (variance)
  - Void/cancelled transaction count and amount
  - Discount total
- [ ] **POS-ERP-02:** If `ShiftSummary` does not cover daily aggregation, add `DailyClosing` Ent schema:
  - Fields: `id`, `outlet_id`, `date`, `total_sales`, `total_refunds`, `total_discounts`, `total_voids`, `cash_expected`, `cash_actual`, `variance`, `status` (open/closed/reconciled), `closed_by`, `notes`
  - Edges: shift_summaries (aggregates all shifts for that outlet+date)
- [ ] **POS-ERP-03:** Generate Atlas migration if new schema added
- [ ] **POS-ERP-04:** Add daily closing handler
  - `POST /api/v1/{tenant}/outlets/{outlet_id}/daily-close` — trigger daily close (aggregates all open shifts)
  - `GET /api/v1/{tenant}/outlets/{outlet_id}/daily-closings` — list daily closing reports
  - `GET /api/v1/{tenant}/daily-closings/{id}` — get daily closing detail
- [ ] **POS-ERP-05:** Add daily closing service logic
  - Aggregate all ShiftSummary records for the outlet+date
  - Calculate variance (expected vs actual)
  - Flag discrepancies above configurable threshold
  - Publish `pos.daily_closing.completed` event

---

## Gap 2: Sale Return / Exchange Workflow

**ERP source:** `ecommerce/pos/` — return/exchange logic in POS order processing
**Priority:** P1
**Status:** Pending

### Current State

pos-api supports `void` (cancel before payment) and `refund` (post-payment via treasury-api). There is no structured return or exchange flow where a customer brings back an item and either gets a refund or swaps for another item.

### Required

- [ ] **POS-ERP-06:** Add `POSReturn` Ent schema
  - Fields: `id`, `original_order_id` (FK), `outlet_id`, `staff_id`, `return_type` (refund/exchange), `status` (pending/approved/completed), `reason`, `total_refund_amount`, `created_at`, `completed_at`
  - Edges: return_lines, exchange_order (optional FK to new POS order for exchanges)
- [ ] **POS-ERP-07:** Add `POSReturnLine` Ent schema
  - Fields: `id`, `pos_return_id` (FK), `original_order_line_id` (FK), `quantity`, `condition`, `refund_amount`
- [ ] **POS-ERP-08:** Generate Atlas migration for return schemas
- [ ] **POS-ERP-09:** Add return handlers
  - `POST /api/v1/{tenant}/pos-orders/{order_id}/returns` — initiate return
  - `GET /api/v1/{tenant}/pos-returns` — list returns (filterable by outlet, date, status)
  - `GET /api/v1/{tenant}/pos-returns/{id}` — get return detail
  - `PATCH /api/v1/{tenant}/pos-returns/{id}/approve` — approve return
- [ ] **POS-ERP-10:** Add return service logic
  - Validate return eligibility (configurable return window, receipt required)
  - On approval with refund: call treasury-api `POST /refunds` with original `payment_intent_id`
  - On approval with exchange: create new POS order, net the price difference
  - Publish `pos.return.completed` event for inventory-api to restock
  - Update shift summary with return amounts

### Events

- `pos.return.initiated` — audit trail
- `pos.return.completed` — inventory-api restocks; treasury-api processes refund
- `pos.exchange.completed` — inventory-api adjusts stock (return old + consume new)

---

## Gap 3: Receipt PDF Generation Verification

**ERP source:** `ecommerce/pos/` — receipt generation (thermal printer format + PDF)
**Priority:** P2
**Status:** Pending — verify existing implementation

### Current State

pos-api may already generate receipts via the POS frontend or a print endpoint. The ERP module had server-side receipt PDF generation for email/download.

### Required

- [ ] **POS-ERP-11:** Audit current receipt generation in pos-api
  - Check if `GET /api/v1/{tenant}/pos-orders/{id}/receipt` endpoint exists
  - Check if PDF generation is implemented (or only thermal printer ESC/POS format)
- [ ] **POS-ERP-12:** If PDF receipt is missing, add:
  - `GET /api/v1/{tenant}/pos-orders/{id}/receipt?format=pdf` — generate PDF receipt
  - Include: outlet name/address, date/time, items with prices, taxes, payment method, total, receipt number
  - Support tenant branding (logo, colors from auth-api tenant cache)
- [ ] **POS-ERP-13:** Verify thermal receipt format covers eTIMS requirements
  - KRA PIN, eTIMS invoice number, QR code (if applicable)

---

## References

- [ERP Module Removal Plan](../../../../erp/erp-api/docs/module-removal-plan.md)
- [Cross-Service Data Ownership](../../../../shared-docs/CROSS-SERVICE-DATA-OWNERSHIP.md)
- [POS Integrations](../integrations.md)
