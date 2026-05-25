# Sprint 11: Reporting & Analytics — pos-api

**Status:** ✅ Core KPIs + Top Items + Staff Sales Delivered — sales-summary, refund-summary, daily-breakdown, top-items, sales-by-staff endpoints shipped; DailyClosing extended with payment-method and order aggregation fields; full EOD reconciliation entity, shift reports, exports, and pre-aggregation worker pending  
**Period:** November–December 2026  
**Last updated:** 2026-05-25  
**Goal:** End-of-day reconciliation, sales reports, staff performance dashboards, export (CSV/PDF), and embedded analytics

---

## Context

Reporting is the most-used feature by business owners and managers:
- End-of-day (EOD) report: total sales, cash vs card vs mobile, discounts, refunds, taxes, net
- Shift report: per-cashier sales summary + cash drawer reconciliation
- Product report: top sellers, slow movers, items with high discount rates
- Staff report: services performed, commissions earned, tips (where applicable)
- Daily/weekly/monthly trend charts for revenue, order volume, average basket size

Reports should be fast (<2s for most), pre-aggregated where possible, and exportable.

---

## Deliverables

### End-of-Day Report
- [ ] `DailyReconciliation` schema — `id, tenant_id, outlet_id, date, total_gross_sales, total_discounts, total_refunds, total_net_sales, total_tax, total_cash, total_card, total_mpesa, total_loyalty_points_redeemed, total_room_charge, total_orders, total_items_sold, closed_by (user_id), closed_at, notes, status enum(open|closed|disputed)`
- [ ] `POST /{tenant}/pos/reports/eod/close` — close the day: compute all figures from today's orders and payments, create `DailyReconciliation`, close all open drawers
- [ ] `GET /{tenant}/pos/reports/eod/{date}` — retrieve EOD report for a specific date
- [ ] `GET /{tenant}/pos/reports/eod` — list EOD reports (date range filter)
- [ ] Cannot close a day that is already closed; superuser can reopen with reason

### Shift Report
- [ ] `GET /{tenant}/pos/reports/shifts/{session_id}` — per-device-session report: sales, payments by tender, cash expected vs actual, voids, refunds
- [ ] `GET /{tenant}/pos/reports/shifts` — all shifts (filter: device_id, user_id, date range)
- [ ] Auto-generate shift summary on `POST /{tenant}/pos/devices/{id}/sessions/close`

### Sales Summary Report
- [ ] `GET /{tenant}/pos/reports/sales/summary` — aggregated sales by date range
  - Response: total_revenue, total_orders, avg_basket_size, by_tender (cash/card/mpesa/...), by_outlet, by_hour_of_day
- [x] `GET /{tenant}/pos/reports/top-items` — items sold with quantity and revenue (filter: date range, limit) — `reports.TopItems`
- [ ] `GET /{tenant}/pos/reports/sales/by-category` — category-level rollup
- [x] `GET /{tenant}/pos/reports/sales-by-staff` — per-staff sales (for commission verification) — `reports.SalesByStaff`
- [ ] `GET /{tenant}/pos/reports/sales/by-hour` — hourly breakdown for a day (heatmap data)

### Staff Performance Report
- [ ] `GET /{tenant}/pos/reports/staff/{staff_id}` — sales, services, commissions, avg service time for a date range
- [ ] `GET /{tenant}/pos/reports/commissions` — unpaid commission totals per staff

### Inventory Movement Report (pos-side snapshot)
- [ ] `GET /{tenant}/pos/reports/stock/consumption` — items consumed (from `StockConsumptionEvent`) for a date range
- [ ] `GET /{tenant}/pos/reports/stock/alerts` — current low-stock alerts from `StockAlertSubscription`

### Tax Report
- [ ] `GET /{tenant}/pos/reports/tax` — tax collected by date range (for KRA / VAT returns)
  - Response: total_taxable_sales, total_tax_amount, by_tax_rate (for multi-rate scenarios)

### Export
- [ ] `GET /{tenant}/pos/reports/{report_type}/export?format=csv&from=...&to=...` — export report as CSV
- [ ] `GET /{tenant}/pos/reports/eod/{date}/export?format=pdf` — PDF export of EOD report (use Go template + wkhtmltopdf or chromedp)

### Pre-Aggregation Worker
- [ ] Background worker: every midnight, pre-aggregate previous day's data into `DailyReconciliation` if not already closed
- [ ] Configurable via `REPORTS_AUTO_CLOSE_EOD=true` env var

### RBAC Additions
- [ ] New permissions: `pos.reports.view`, `pos.reports.export`, `pos.reports.eod_close`
- [ ] `pos.reports.view` assigned to `store_manager`, `pos_admin`, `viewer`
- [ ] `pos.reports.eod_close` assigned to `store_manager`, `pos_admin`
- [ ] `pos.reports.export` assigned to `store_manager`, `pos_admin`

### Implemented (2026-05-21 / updated 2026-05-25)

- [x] `GET /{tenant}/pos/reports/sales-summary` — aggregated sales summary (`reports.go` handler)
- [x] `GET /{tenant}/pos/reports/refund-summary` — refund summary
- [x] `GET /{tenant}/pos/reports/daily-breakdown` — daily breakdown data

### DailyClosing Extension (2026-05-25)

Extended the existing Sprint 5 `DailyClosing` Ent schema with payment-method breakdown and order aggregate fields:
- [x] `total_card` — card tender total for the day
- [x] `total_mpesa` — M-Pesa tender total for the day
- [x] `total_tax` — tax collected total
- [x] `total_loyalty_redemptions` — loyalty points redeemed total
- [x] `total_room_charge` — room-charge tender total (hotel outlets)
- [x] `total_orders` — number of orders closed for the day
- [x] `total_items_sold` — number of line items sold for the day
- [x] `closed_at` — timestamp when the daily closing was finalised

Atlas migration generated for `DailyClosing` extensions.

> **Note:** `DailyClosing` (Sprint 5) is an outlet-level cash-management entity. The planned `DailyReconciliation` is a separate, more granular reporting entity; it remains unimplemented.

### Migration
- [ ] `DailyReconciliation` ent schema — not added (separate from `DailyClosing`)
- [ ] Atlas migration: `reporting_module` — pending
- [ ] `docs/erd.md` updated — pending

## Completion Notes (2026-05-25)

Report endpoints: `sales-summary`, `refund-summary`, `daily-breakdown`, `top-items`, `sales-by-staff` live. `DailyClosing` extended with payment-method and order aggregate fields. Full EOD lifecycle (`DailyReconciliation` entity, EOD close/reopen, shift reports, per-item/per-staff/per-hour breakdowns, tax report, CSV/PDF export, pre-aggregation background worker) remains unimplemented.

---

## Use Cases Covered

| Report | Business Types |
|--------|---------------|
| End-of-day reconciliation | All business types |
| Shift cash reconciliation | Retail, restaurant, hotel, pharmacy |
| Top-selling items | Supermarket, restaurant, pharmacy |
| Staff commission report | Salon, barbershop, spa, service |
| Hourly sales heatmap | Restaurant, bar, retail |
| Tax collection report | All businesses (VAT/KRA compliance) |
| Stock consumption report | Restaurant, pharmacy, retail |
| CSV/PDF export | All businesses (accounting, audits) |
