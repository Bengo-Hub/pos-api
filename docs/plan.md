# Codevertex POS — Use Case & Workflow Reference

**Last updated:** 2026-05-21  
**Scope:** pos-api, pos-ui, cafe-website integration  
**Research basis:** Codevertex Africa POS Market Research (May 2026) + hotel-pos-v8.jsx design prototype

This document describes every business vertical the POS supports, the workflows each enables, and the features required. It links to sprint files, ERD, and integration docs.

---

## Supported Business Verticals

| Vertical | Examples | Use Case Config | Priority |
|----------|---------|-----------------|----------|
| Restaurant | Dine-in, fast food, food court | `hospitality` | ✅ Core |
| Bar / Grill | Sports bar, cocktail lounge, rooftop | `hospitality` | ✅ Core |
| Hotel / Lodge | Full-service hotel, boutique lodge | `hospitality` | ✅ Sprint 3 |
| Supermarket | Grocery, mini-mart, convenience | `retail` | Sprint 7 |
| Hardware / Electronics | Hardware store, electronics shop | `retail` | Sprint 7 |
| Pharmacy | Community pharmacy, hospital dispensary | `pharmacy` | Sprint 8 |
| Barbershop / Salon | Hair salon, nail bar, beauty studio | `services` | Sprint 9 |
| Clinic / Spa | Physiotherapy, massage, dental | `services` | Sprint 9 |
| Car Wash | Express wash, detailing centre | `services` | Sprint 9 |
| Hybrid | Hotel restaurant + bar + spa | `hospitality` | ✅ Core |

---

## Kenya-Specific Must-Haves

These are non-negotiable for any Kenyan deployment. Missing any of these makes the product non-viable.

| Requirement | Implementation | Status |
|-------------|---------------|--------|
| M-Pesa STK Push | Treasury-api handles Daraja; pos-api creates payment intent | ⚠️ Intent endpoint registered; NATS subscriber not wired (Sprint 6) |
| KRA eTIMS Compliance | OSCU/VSCU modes, offline queue, auto-transmission | ❌ Not implemented (Sprint 12) |
| Offline Mode | IndexedDB on pos-ui, offline order queue, sync on reconnect | ✅ pos-ui IndexedDB + sync worker complete (Sprint 6) |
| Multi-branch Management | Tenant/outlet scoping on all entities | ✅ Complete |
| KES Currency | `DEFAULT_CURRENCY=KES`, `TAX_RATE_PERCENT=16` | ✅ Configurable |
| Receipt Generation | Till reports, regulatory exports | 🟡 Receipt endpoint exists; PDF format pending |

---

## Hospitality Use Cases

### Restaurant

**Workflow — Dine-In:**
1. Waiter opens POS terminal, selects outlet and shift
2. Opens floor plan, selects available table, sets cover count
3. Adds menu items (with modifiers, special instructions)
4. Sends to kitchen: KDS tickets created per station (kitchen, grill)
5. Kitchen marks tickets In Progress → Ready
6. Waiter marks order served
7. Customer requests bill; cashier presents total with VAT breakdown
8. Payment: cash, card, M-Pesa, or split tender
9. Receipt printed / SMS / eTIMS QR code
10. Table released to available

**Key Features:** floor plan, course firing, bar tabs, order split, modifiers, KDS, void/return

**Sprints:** [1](sprints/sprint-1-foundation.md) · [2](sprints/sprint-2-orders-catalog.md) · [4](sprints/sprint-4-kds-bar.md) · [5](sprints/sprint-5-erp-gaps.md)

---

### Bar / Grill

**Workflow — Bar Tab:**
1. Bartender opens tab (customer name or table)
2. Drinks/food added throughout the evening
3. Items appear on bar KDS (filtered to bar station)
4. Customer settles tab (cash, card, room charge if hotel)

**Key Features:** bar tab management, bar KDS, happy hour pricing, age verification (licensing compliance)

**Sprints:** [2](sprints/sprint-2-orders-catalog.md) · [4](sprints/sprint-4-kds-bar.md) · [10](sprints/sprint-10-loyalty-promotions.md)

---

### Hotel / Lodge

**Workflow — Guest Check-In:**
1. Receptionist opens Hotel module
2. Selects available room, enters guest details (name, ID, phone)
3. Sets number of nights; system calculates check-out date and room charge
4. First room charge posted to folio; room status → Occupied

**Workflow — Room Service:**
1. Guest calls; order entered in POS with room number context
2. Routed to kitchen KDS; delivered to room
3. Charge auto-posted to room folio (tender = room charge)

**Workflow — Guest Check-Out:**
1. Receptionist opens room folio: room charges + food + laundry + minibar
2. Reviews total; applies corrections
3. Guest settles: cash, card, M-Pesa, or company account
4. Room → Cleaning; housekeeping notified

**Workflow — Facility Booking:**
1. Guest/receptionist books pool, gym, conference room, or spa
2. Session fee added to folio or paid immediately
3. Facility status updated

**Key Features:** room grid (6 statuses), folio (5 charge types), multi-night automation, facility booking calendar, room-charge tender, bulk settlement at check-out via treasury

**Sprints:** [3](sprints/sprint-3-hotel-module.md) · [6](sprints/sprint-6-inventory-treasury.md)

---

## Retail Use Cases

### General Retail / Supermarket / Hardware

**Workflow — Standard Sale:**
1. Cashier opens retail POS (no table context)
2. Scans barcode or enters SKU; item added with current price and stock level
3. Weight-based items: scale reading captured before cart add
4. Promotions applied automatically (BXGY, multi-buy)
5. Payment collected; receipt printed with eTIMS QR
6. Stock consumption event published to inventory-api

**Workflow — Layaway:**
1. Customer selects items, cannot pay in full
2. Cashier converts cart to layaway plan; initial deposit collected
3. Customer makes instalment payments on subsequent visits
4. On full payment: order completed, items reserved

**Key Features:** barcode/EAN/QR scan (< 100ms), weight-based pricing, serial number capture, layaway plans, customer-facing pole display, real-time stock, low-stock warnings, out-of-stock override with manager PIN

**Sprints:** [7 API](sprints/sprint-7-retail-module.md) · [7 UI](../../pos-ui/docs/sprints/sprint-7-retail-ui.md)

---

## Pharmacy Use Cases

**Workflow — Prescription Dispensing:**
1. Patient presents prescription; pharmacist logs it
2. Items added to cart; system validates prescription validity
3. Controlled substances: second pharmacist witness required; dispensing register auto-created
4. Lot/batch selected by FIFO; expiry date and lot number captured per line
5. NHIF co-pay applied if applicable
6. Payment; receipt includes prescription number

**Workflow — OTC Age-Restricted Sale:**
1. Cashier scans item; system flags age restriction
2. Cashier checks customer ID; logs verification method
3. Sale proceeds with verification record on order line

**Key Features:** prescription registration/validation/filling, controlled substance register, age verification logging, lot/batch tracking with expiry at line level, partial pack dispensing, non-returnable enforcement, NHIF processing, patient profiles

**Sprints:** [8 API](sprints/sprint-8-pharmacy-module.md)

---

## Service Business Use Cases

### Barbershop / Salon

**Workflow — Appointment Visit:**
1. Client books appointment (walk-in or pre-booked)
2. Stylist assigned; client checked in on arrival
3. Service started (timer begins)
4. Service completed; POS order created automatically
5. Payment; commission calculated for stylist

**Key Features:** appointment calendar, client preference cards, walk-in queue with estimated wait, commission rules per service, service packages with session balances

**Sprints:** [9 API](sprints/sprint-9-service-module.md) · [8 UI](../../pos-ui/docs/sprints/sprint-8-service-ui.md)

---

### Car Wash

**Workflow:**
1. Customer drives in; attendant adds to queue (vehicle type, wash type)
2. Bay assigned when available
3. Wash completed; customer notified
4. Payment at cashier; receipt

**Key Features:** bay status display, vehicle type pricing, wash package bundles, queue wait time

---

## Cross-Cutting Features

### Loyalty & Promotions (Sprint 10)
- Points earned on every purchase; balance shown at checkout
- Customer lookup by phone; points as tender
- Tier upgrades (Bronze → Silver → Gold) unlock price books
- Time-window discounts (happy hour), multi-buy, bundle pricing

### Reporting & End-of-Day (Sprint 11)
- Cashier closes shift: cash count entered, reconciliation auto-computed
- Manager closes day: all shifts summarised, EOD report
- Accountant exports CSV or PDF
- KRA VAT tax report for compliance

### KRA eTIMS Compliance (Sprint 12)
- OSCU mode: online transmission of each invoice at order completion
- VSCU mode: offline queue with auto-sync on internet restore
- eTIMS QR code on receipt
- `regulatory_exports` tracking per submission

### External Integrations (Sprint 12)
- Online ordering (Uber Eats/Glovo): order → POS + KDS automatically
- Menu changes pushed to delivery platforms on catalog update
- Sales → Xero/QuickBooks at EOD
- Webhooks for custom integrations (ERP, loyalty platforms)

### Online Ordering → KDS Bridge (Sprint 13)
- `ordering.order.status.changed` subscriber: create KDS ticket when hospitality order reaches `confirmed`/`preparing`
- Completion callback: `pos.kds.ticket.ready` → ordering-backend optional status update

### Offline Operation (pos-ui Sprint 6)
- Device loses internet: POS continues from local cache
- Orders queued in IndexedDB; synced to API on reconnect
- Offline indicator; manager alerted if sync queue exceeds threshold

---

## Sprint Index

### pos-api Sprints

| # | Title | Status | Covers |
|---|-------|--------|--------|
| [1](sprints/sprint-1-foundation.md) | Foundation — Auth, RBAC, Devices | ✅ Complete | All verticals |
| [2](sprints/sprint-2-orders-catalog.md) | Orders, Catalog, Payments, Tables | ✅ Complete | Restaurant, retail |
| [3](sprints/sprint-3-hotel-module.md) | Hotel Module (schema + handlers) | ✅ Complete | Hotel, lodge |
| [4](sprints/sprint-4-kds-bar.md) | KDS & Bar Display (handlers done) | ✅ Complete | Restaurant, bar |
| [5](sprints/sprint-5-erp-gaps.md) | ERP Gaps — Daily Close, Returns, Receipt | ✅ Substantially complete | All verticals |
| [6](sprints/sprint-6-inventory-treasury.md) | Inventory & Treasury Wiring (NATS subscribers) | 🟡 Partial — S2S clients exist; NATS subscribers missing | All verticals |
| [7](sprints/sprint-7-retail-module.md) | Retail Module (layaway, scale, barcode, serial) | ✅ Core delivered | Supermarket, hardware |
| [8](sprints/sprint-8-pharmacy-module.md) | Pharmacy Module (prescriptions, drug checks) | ✅ Core delivered | Pharmacy |
| [9](sprints/sprint-9-service-module.md) | Service Business Module (appointments, schedules, commissions) | ✅ Core delivered | Salon, clinic, car wash |
| [10](sprints/sprint-10-loyalty-promotions.md) | Loyalty Programs, Accounts, Earn/Redeem | ✅ Core delivered | All verticals |
| [11](sprints/sprint-11-reporting-analytics.md) | Reporting — Sales/Refund/Daily KPIs | ✅ Core KPIs delivered | All verticals |
| [12](sprints/sprint-12-integrations-webhooks.md) | Webhook CRUD | 🟡 Subscription CRUD delivered; delivery worker missing | All verticals |
| [13](sprints/sprint-13-ordering-kds-integration.md) | Online Order Pickup Endpoints | 🟡 Pickup endpoints delivered; NATS KDS subscriber missing | Restaurant, bar, hotel |

### pos-ui Sprints

| # | Title | Status | Covers |
|---|-------|--------|--------|
| [1](../../pos-ui/docs/sprints/sprint-1-mvp-foundation.md) | Foundation — Scaffold, Auth, Layout | ✅ Complete | All verticals |
| [2](../../pos-ui/docs/sprints/sprint-2-order-entry.md) | Order Entry — Menu Grid, Cart, Payment | ✅ Complete | Restaurant, retail |
| [3](../../pos-ui/docs/sprints/sprint-3-tables-shifts.md) | Tables, Floor Plan, Shifts | ✅ Complete | Restaurant, hotel |
| [4](../../pos-ui/docs/sprints/sprint-4-hotel.md) | Hotel UI — Rooms, Facilities | 🟡 Scaffold done — API hooks not wired | Hotel, lodge |
| [5](../../pos-ui/docs/sprints/sprint-5-kds.md) | KDS Terminal View | ✅ Complete | Restaurant, bar |
| [6](../../pos-ui/docs/sprints/sprint-6-offline.md) | Offline / PWA (IndexedDB, sync worker) | ✅ Complete | All verticals |
| [7](../../pos-ui/docs/sprints/sprint-7-retail-ui.md) | Retail UI (barcode, layaway, serial) | ✅ Core delivered | Supermarket, hardware |
| [8](../../pos-ui/docs/sprints/sprint-8-service-ui.md) | Service Business UI (appointments, commissions) | ✅ Core delivered | Salon, clinic, car wash |
| [9](../../pos-ui/docs/sprints/sprint-9-reports-ui.md) | Reports & Analytics UI | ✅ Core delivered | All verticals |
| [10](../../pos-ui/docs/sprints/sprint-10-pos-auth.md) | Dual Auth — SSO + PIN Terminal Login | ✅ Complete | All verticals |

---

## Key Documentation

| Document | Purpose |
|----------|---------|
| [ERD](erd.md) | Entity definitions, relationships, and field descriptions |
| [Integrations](integrations.md) | S2S API contracts, NATS events, treasury/inventory wiring |
| [Architecture](architecture.md) | Layer overview, auth, RBAC, Kenya requirements |
| [pos-api sprints](sprints/) | Sprint-by-sprint task tracking for the Go API |
| [pos-ui sprints](../../pos-ui/docs/sprints/) | Sprint-by-sprint task tracking for the Next.js PWA |
| [Design reference](../../pos-ui/docs/use-case-designs/hotel-pos-v8.jsx) | Full UX prototype for hotel + restaurant + bar POS |
