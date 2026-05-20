# Sprint: ERP E-commerce Gaps — pos-api

**Created:** April 2026
**Status:** In Progress (Gaps 1 & 2 complete, Gap 3 pending)
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

- [x] **POS-ERP-01:** Audit `ShiftSummary` schema — no ShiftSummary found; DailyClosing added instead
- [x] **POS-ERP-02:** Added `DailyClosing` Ent schema
  - Fields: `id`, `outlet_id`, `business_date`, `total_sales`, `total_refunds`, `total_discounts`, `total_voids`, `cash_expected`, `cash_actual`, `variance`, `status` (open/closed/reconciled), `closed_by`, `notes`, `drawer_ids`
  - Aggregates CashDrawer + POSOrder + POSRefund rows for the outlet+date
- [x] **POS-ERP-03:** Atlas migration `20260520_erp_returns_closings.sql`
- [x] **POS-ERP-04:** Added daily closing handler (`internal/http/handlers/closings.go`)
  - `POST /{tenant}/pos/outlets/{outletID}/daily-close`
  - `GET /{tenant}/pos/outlets/{outletID}/daily-closings`
- [x] **POS-ERP-05:** Closing logic: aggregates drawers + orders + refunds, computes variance

---

## Gap 2: Sale Return / Exchange Workflow

**ERP source:** `ecommerce/pos/` — return/exchange logic in POS order processing
**Priority:** P1
**Status:** Pending

### Current State

pos-api supports `void` (cancel before payment) and `refund` (post-payment via treasury-api). There is no structured return or exchange flow where a customer brings back an item and either gets a refund or swaps for another item.

### Required

- [x] **POS-ERP-06:** Added `POSReturn` Ent schema (`internal/ent/schema/posreturn.go`)
  - Fields: `id`, `order_id`, `outlet_id`, `return_number`, `return_type` (refund/exchange/store_credit), `status` (pending/approved/rejected/completed), `reason`, `refund_amount`, `exchange_order_id`, `requested_by`, `approved_by`, `treasury_refund_ref`
- [x] **POS-ERP-07:** Added `POSReturnLine` Ent schema (`internal/ent/schema/posreturnline.go`)
  - Fields: `id`, `return_id`, `order_line_id`, `sku`, `name`, `quantity`, `unit_price`, `total_price`, `reason`
- [x] **POS-ERP-08:** Atlas migration included in `20260520_erp_returns_closings.sql`
- [x] **POS-ERP-09:** Added return handlers (`internal/http/handlers/returns.go`)
  - `POST /{tenant}/pos/orders/{orderID}/returns` — initiate return
  - `GET /{tenant}/pos/returns` — list returns (filterable by status)
  - `PATCH /{tenant}/pos/returns/{returnID}/approve` — manager approval/rejection
- [ ] **POS-ERP-10:** Return service logic (treasury refund call + inventory restock event) — pending
  - On approval with refund: call treasury-api `POST /refunds`
  - Publish `pos.return.completed` event for inventory-api to restock

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
