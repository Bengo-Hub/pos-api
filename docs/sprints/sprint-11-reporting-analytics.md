# Sprint 11: Reporting & Analytics — pos-api

**Status:** 🔴 Not Started  
**Period:** November–December 2026  
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
- [ ] `GET /{tenant}/pos/reports/sales/by-item` — items sold with quantity and revenue (filter: date range, category, outlet)
- [ ] `GET /{tenant}/pos/reports/sales/by-category` — category-level rollup
- [ ] `GET /{tenant}/pos/reports/sales/by-staff` — per-staff sales (for commission verification)
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

### Migration
- [ ] Add `DailyReconciliation` ent schema
- [ ] Run `go generate ./internal/ent`
- [ ] Generate Atlas migration: `reporting_module`
- [ ] Update `docs/erd.md`

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
