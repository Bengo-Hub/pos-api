# Sprint 5: ERP Gaps — pos-api

**Status:** 🟡 Planned  
**Period:** June 2026  
**Goal:** Close feature gaps identified from ERP module audit — daily closing, sale returns/exchanges, receipt PDF

---

## Context

Merged from `sprint-erp-gaps.md` (April 2026). These features existed in the legacy ERP `ecommerce/pos/` module and must be implemented in pos-api before the ERP module is deleted.

---

## Gap 1: Daily Closing Reconciliation Report

**Priority:** P1

### Current State
`pos_device_sessions` and `cash_drawer` track shift-level totals. No daily aggregation entity.

### Required
- [ ] Audit `POSDeviceSession` schema — confirm it captures:
  - Total sales by payment method (cash, card, M-Pesa, split)
  - Total refunds/returns, expected vs actual cash, void count, discount total
- [ ] If missing: Add `DailyClosing` Ent schema:
  - Fields: id, tenant_id, outlet_id, date, total_sales, total_refunds, total_discounts, total_voids, cash_expected, cash_actual, variance, status (open/closed/reconciled), closed_by, notes
  - Edges: outlet, shifts (aggregated sessions)
- [ ] Run Atlas migration if schema added: `go run cmd/migrate/main.go daily_closing`
- [ ] Add endpoints:
  - `POST /{tenant}/outlets/{outlet_id}/daily-close` — trigger daily close (aggregates all shifts)
  - `GET /{tenant}/outlets/{outlet_id}/daily-closings` — list reports
  - `GET /{tenant}/daily-closings/{id}` — detail
- [ ] Service logic: aggregate sessions, calculate variance, flag discrepancies, publish `pos.daily_closing.completed`

---

## Gap 2: Sale Return / Exchange Workflow

**Priority:** P1

### Current State
pos-api supports `void` (pre-payment cancel) and `refund` (post-payment via treasury). No structured return/exchange flow.

### Required
- [ ] Add `POSReturn` Ent schema:
  - Fields: id, tenant_id, original_order_id (FK), outlet_id, staff_id, return_type (refund/exchange), status (pending/approved/completed), reason, total_refund_amount, created_at, completed_at
- [ ] Add `POSReturnLine` Ent schema:
  - Fields: id, pos_return_id (FK), original_order_line_id (FK), quantity, condition, refund_amount
- [ ] Run Atlas migration: `go run cmd/migrate/main.go pos_returns`
- [ ] Add endpoints:
  - `POST /{tenant}/pos-orders/{order_id}/returns` — initiate return
  - `GET /{tenant}/pos-returns` — list returns (filter: outlet, date, status)
  - `GET /{tenant}/pos-returns/{id}` — detail
  - `PATCH /{tenant}/pos-returns/{id}/approve` — approve return
- [ ] Service logic:
  - Validate return window (configurable, from service_configs)
  - On refund approval: call treasury-api `POST /refunds` with original payment_intent_id
  - On exchange: create new POS order, net price difference
  - Publish `pos.return.completed` → inventory-api restocks
  - Update session summary with return amounts

**Events Published:**
- `pos.return.initiated` — audit trail
- `pos.return.completed` — inventory restocks, treasury processes refund
- `pos.exchange.completed` — inventory adjusts stock

---

## Gap 3: Receipt PDF Generation

**Priority:** P2

### Current State
Thermal receipt format may exist. No server-side PDF generation confirmed.

### Required
- [ ] Audit: check if `GET /{tenant}/pos-orders/{id}/receipt` endpoint exists
- [ ] If PDF missing, add:
  - `GET /{tenant}/pos-orders/{id}/receipt?format=pdf` — generate PDF receipt
  - Include: outlet name/address, date/time, items with prices, taxes, payment method, total, receipt number
  - Support tenant branding (logo from auth-api tenant cache)
- [ ] Verify thermal receipt format covers eTIMS requirements (KRA PIN, eTIMS invoice number, QR code)

---

## Tasks
- [ ] Audit POSDeviceSession coverage for daily closing
- [ ] Add DailyClosing schema if needed + migration
- [ ] Add DailyClosing handler + service
- [ ] Add POSReturn + POSReturnLine schemas + migration
- [ ] Add return/exchange handler + service
- [ ] Wire treasury refund call on return approval
- [ ] Add receipt PDF endpoint
- [ ] Update Swagger: `swag init`
- [ ] Build and fix all errors: `go build ./...`
- [ ] Push to staging, merge to main
