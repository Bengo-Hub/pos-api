# BengoBox POS — Use Case & Workflow Reference

**Last updated:** 2026-05-07  
**Scope:** pos-api, pos-ui, cafe-website integration

This document describes every business vertical the POS supports, the workflows each enables, and the features required. It links to sprint files, ERD, and integration docs. It is not a coding guide.

---

## Supported Business Verticals

| Vertical | Examples | Mode |
|----------|---------|------|
| Restaurant | Dine-in, fast food, food court | Hospitality |
| Bar / Grill | Sports bar, cocktail lounge, rooftop | Hospitality |
| Hotel / Lodge | Full-service hotel, boutique lodge, resort | Hospitality |
| Supermarket | Grocery, mini-mart, convenience | Retail |
| Hardware / General Retail | Hardware store, electronics shop | Retail |
| Pharmacy | Community pharmacy, hospital dispensary | Retail (regulated) |
| Barbershop / Salon | Hair salon, nail bar, beauty studio | Service |
| Clinic / Spa | Physiotherapy, massage, dental | Service |
| Car Wash | Express wash, detailing centre | Service |
| Any hybrid | Hotel restaurant + bar + spa | Multi-mode |

---

## Hospitality Use Cases

### Restaurant

**Who uses it:** Waiter, cashier, kitchen staff, manager

**Workflow — Dine-In Order:**
1. Waiter opens POS terminal, selects outlet and shift
2. Opens floor plan, selects an available table, sets cover count
3. Adds menu items to the order (with modifiers — e.g., extra sauce, no onions)
4. Sends order to kitchen: KDS tickets created per station (kitchen, grill)
5. Kitchen marks tickets In Progress → Ready
6. Waiter marks order served
7. Customer requests bill; cashier presents total with tax breakdown
8. Payment collected: cash, card, M-Pesa, or split tender
9. Receipt printed / sent via SMS
10. Table released back to available

**Workflow — Takeaway Order:**
- Same as dine-in but no table assignment; order type = takeaway
- Order may come from online ordering channel (appears in POS via channel integration)

**Key Features:**
- Floor plan with table status (available, occupied, reserved, bill-pending)
- Course-based ordering (starter, main, dessert — fire by course)
- Open bar tabs linked to a table or customer
- Order split (split by item, split equally)
- Modifier groups (required, optional, multi-select)
- Discount application at line or order level
- Void and return for incorrect items
- Kitchen Display System (KDS) with station routing

**Sprints:** [Sprint 1](sprints/sprint-1-foundation.md) · [Sprint 2](sprints/sprint-2-orders-catalog.md) · [Sprint 4](sprints/sprint-4-kds-bar.md) · [Sprint 5](sprints/sprint-5-erp-gaps.md)

---

### Bar / Grill

**Who uses it:** Bartender, bar back, grill cook, manager

**Workflow — Bar Tab:**
1. Bartender opens a bar tab for a customer (name or table)
2. Drinks and food items added to tab throughout the evening
3. Tab displayed on bar KDS (filtered to bar station)
4. Bartender calls "Ready" when drinks are prepared
5. Customer settles tab at end of evening (cash, card, room charge if hotel)
6. Bar tab closed and linked POS order completed

**Key Features:**
- Bar tab management (open, add items, merge, close)
- Bar KDS display: separate from kitchen, filtered to beverages and bar food
- Happy hour pricing (time-window promotions)
- Age verification at point of first drink (for licensing compliance)
- Staff drinks logging (for cost control)

**Sprints:** [Sprint 2](sprints/sprint-2-orders-catalog.md) · [Sprint 4](sprints/sprint-4-kds-bar.md) · [Sprint 10](sprints/sprint-10-loyalty-promotions.md)

---

### Hotel / Lodge

**Who uses it:** Receptionist, restaurant cashier, facilities attendant, manager

**Workflow — Guest Check-In:**
1. Receptionist opens Hotel module on POS
2. Selects an available room, enters guest details (name, ID, phone)
3. Sets number of nights; system calculates check-out date and room charge
4. First room charge posted to guest folio
5. Room status changes to Occupied

**Workflow — Room Service Order:**
1. Guest calls room service; order entered in POS with room context
2. Order routed to kitchen KDS; delivered to room
3. Charge posted to room folio automatically (tender = room charge)

**Workflow — Guest Check-Out:**
1. Receptionist opens room folio: room charges + food + laundry + minibar
2. Reviews total with guest; applies any corrections
3. Guest settles: cash, card, M-Pesa, or company account
4. Room status changes to Cleaning; housekeeping notified

**Workflow — Facility Booking:**
1. Guest or receptionist books pool, gym, conference room, or spa slot
2. Session fee added to guest folio or paid immediately
3. Facility status updated (occupied during session)

**Key Features:**
- Room grid with 6 statuses: available, occupied, cleaning, maintenance, reserved, checkout
- Room folio with charge types: room, food, laundry, minibar, room service, other
- Multi-night stays with nightly charge automation
- Facility booking calendar (pool, gym, conference, spa, kids area)
- Room charge tender type for restaurant and bar orders
- Bulk folio settlement at check-out via treasury payment intent

**Sprints:** [Sprint 3](sprints/sprint-3-hotel-module.md) · [Sprint 6](sprints/sprint-6-inventory-treasury.md)

---

## Retail Use Cases

### General Retail / Supermarket / Hardware

**Who uses it:** Cashier, stock clerk, store manager

**Workflow — Standard Sale:**
1. Cashier opens retail POS terminal (no table context)
2. Scans barcode or enters SKU; item added to cart with current price and stock level
3. Weight-based items: scale reading captured before adding to cart
4. Cart reviewed; promotions applied automatically (BXGY, multi-buy)
5. Payment collected; receipt printed
6. Stock consumption event published to inventory-api

**Workflow — Layaway Sale:**
1. Customer selects items; cannot pay in full today
2. Cashier converts cart to a layaway plan; initial deposit collected
3. Customer makes instalment payments on subsequent visits
4. On full payment: order completed, items reserved

**Key Features:**
- Instant barcode / EAN / QR lookup with sub-100ms response
- Weight-based pricing (produce, deli, bulk goods)
- Serial number capture at checkout (electronics, appliances)
- Layaway / deferred payment plans with instalment tracking
- Customer-facing display (pole display) mirroring cart
- Real-time stock levels with low-stock warnings
- Out-of-stock override with manager PIN

**Sprints:** [Sprint 7 (API)](sprints/sprint-7-retail-module.md) · [Sprint 7 (UI)](../../pos-ui/docs/sprints/sprint-7-retail-ui.md)

---

### Pharmacy

**Who uses it:** Pharmacist, pharmacy technician, cashier, dispensary manager

**Workflow — Prescription Dispensing:**
1. Patient presents prescription; pharmacist logs it in the system
2. Items on prescription added to cart; system validates prescription is valid and not expired
3. Controlled substances: second pharmacist witness required; dispensing register auto-created
4. Lot/batch selected by FIFO; expiry date and lot number captured per line
5. Payment collected; receipt includes prescription number

**Workflow — OTC Age-Restricted Sale:**
1. Cashier scans item; system flags age restriction
2. Cashier checks customer ID and logs verification method
3. Sale proceeds with verification record attached to order line

**Key Features:**
- Prescription registration, validation, and filling workflow
- Controlled substance dispensing register (regulatory requirement)
- Age verification logging per transaction
- Lot/batch tracking with expiry dates at the line level
- Partial pack dispensing (sell individual tablets from a box)
- Non-returnable item enforcement
- Prescription-only item gate at cart addition

**Sprints:** [Sprint 8 (API)](sprints/sprint-8-pharmacy-module.md)

---

## Service Business Use Cases

### Barbershop / Salon

**Who uses it:** Stylist, receptionist, cashier, owner

**Workflow — Appointment Visit:**
1. Client books appointment (walk-in or pre-booked)
2. Stylist is assigned; client checked in on arrival
3. Service started (timer begins)
4. Service completed; POS order created automatically
5. Payment collected; commission calculated for stylist

**Workflow — Package Sale:**
1. Client purchases a package (e.g., 10 haircuts at a discounted rate)
2. Each visit: receptionist looks up client by phone, redeems one session
3. Package balance tracked; alert when sessions running low

**Key Features:**
- Appointment calendar with staff columns and time slots
- Client preference cards (style notes, product preferences)
- Walk-in queue with estimated wait time
- Commission rules per service per staff member
- Service packages with session balances
- Staff availability grid for booking

**Sprints:** [Sprint 9 (API)](sprints/sprint-9-service-module.md) · [Sprint 8 (UI)](../../pos-ui/docs/sprints/sprint-8-service-ui.md)

---

### Clinic / Physiotherapy / Spa

**Who uses it:** Therapist, receptionist, billing clerk, practitioner

**Workflow — Patient Visit:**
1. Receptionist books appointment (practitioner, treatment, duration)
2. Patient checks in; treatment record opened
3. Practitioner marks session complete; notes added (if clinic)
4. POS order created; treatment fee charged
5. Insurance or package redemption applied if applicable

**Key Features:**
- Appointment booking with duration and resource (treatment room) conflict checking
- Client health records / visit notes
- Service packages and pre-paid treatment plans
- Staff commission on services
- Referral tracking between practitioners

**Sprints:** [Sprint 9 (API)](sprints/sprint-9-service-module.md) · [Sprint 8 (UI)](../../pos-ui/docs/sprints/sprint-8-service-ui.md)

---

### Car Wash

**Who uses it:** Attendant, cashier, manager

**Workflow — Walk-In:**
1. Customer drives in; attendant adds to queue (vehicle type, wash type)
2. Bay assignment when a bay is free
3. Wash started; timer runs
4. Wash completed; customer notified
5. Payment at cashier or terminal; receipt issued

**Key Features:**
- Walk-in queue with bay assignment
- Bay status display (free, occupied, drying)
- Vehicle type pricing (sedan, SUV, truck)
- Wash package bundles (e.g., 10-wash card)
- Queue estimated wait time display

**Sprints:** [Sprint 9 (API)](sprints/sprint-9-service-module.md) · [Sprint 8 (UI)](../../pos-ui/docs/sprints/sprint-8-service-ui.md)

---

## Cross-Cutting Features

### Loyalty & Promotions

Applies to all business types.

**Workflows:**
- Customer earns points on every purchase; points balance shown at checkout
- Cashier looks up customer by phone before payment; applies points as tender
- Tier upgrades (Bronze → Silver → Gold) unlock price books or discount rates
- Manager configures time-window discounts (happy hour), multi-buy promotions, and bundle pricing

**Sprints:** [Sprint 10](sprints/sprint-10-loyalty-promotions.md)

---

### Reporting & End-of-Day

Applies to all business types.

**Workflows:**
- Cashier closes shift: cash count entered, reconciliation auto-computed
- Manager closes the day: all shifts summarised, EOD report generated
- Accountant exports CSV or PDF for bookkeeping
- KRA / VAT tax report for compliance filing

**Sprints:** [Sprint 11](sprints/sprint-11-reporting-analytics.md) · [Sprint 9 UI](../../pos-ui/docs/sprints/sprint-9-reports-ui.md)

---

### External Integrations

**Workflows:**
- Online ordering platform (Uber Eats, Glovo) sends order → appears on POS and KDS automatically
- Menu changes pushed to delivery platforms on catalog update
- Sales data pushed to Xero / QuickBooks at EOD
- KRA eTIMS fiscal signing on order completion (Kenya)
- Business event webhooks for custom integrations (own tech stack, ERP, loyalty platforms)

**Sprints:** [Sprint 12](sprints/sprint-12-integrations-webhooks.md)

---

### Offline Operation (pos-ui)

**Workflow:**
- Device loses internet connection; POS continues operating from local cache
- Orders queued in IndexedDB; synced to API when connection restored
- Offline indicator shown; manager alerted if sync queue exceeds threshold

**Sprints:** [Sprint 6 UI](../../pos-ui/docs/sprints/sprint-6-offline.md)

---

## Sprint Index

### pos-api Sprints

| # | Title | Status | Covers |
|---|-------|--------|--------|
| [1](sprints/sprint-1-foundation.md) | Foundation — Auth, RBAC, Devices | ✅ Complete | All verticals |
| [2](sprints/sprint-2-orders-catalog.md) | Orders, Catalog, Payments, Tables | ✅ Complete | Restaurant, retail |
| [3](sprints/sprint-3-hotel-module.md) | Hotel Module | ✅ Schema done, HTTP handlers done | Hotel, lodge |
| [4](sprints/sprint-4-kds-bar.md) | KDS & Bar Display | ✅ Handlers done | Restaurant, bar |
| [5](sprints/sprint-5-erp-gaps.md) | ERP Gaps — Daily Close, Returns | 🟡 Planned | All verticals |
| [6](sprints/sprint-6-inventory-treasury.md) | Inventory & Treasury Wiring | 🟡 Planned | All verticals |
| [7](sprints/sprint-7-retail-module.md) | Retail Module | 🔴 Not started | Supermarket, hardware, general retail |
| [8](sprints/sprint-8-pharmacy-module.md) | Pharmacy Module | 🔴 Not started | Pharmacy |
| [9](sprints/sprint-9-service-module.md) | Service Business Module | 🔴 Not started | Salon, clinic, car wash |
| [10](sprints/sprint-10-loyalty-promotions.md) | Loyalty & Advanced Promotions | 🔴 Not started | All verticals |
| [11](sprints/sprint-11-reporting-analytics.md) | Reporting & Analytics | 🔴 Not started | All verticals |
| [12](sprints/sprint-12-integrations-webhooks.md) | Integrations & Webhooks | 🔴 Not started | All verticals |

### pos-ui Sprints

| # | Title | Status | Covers |
|---|-------|--------|--------|
| [1](../../pos-ui/docs/sprints/sprint-1-mvp-foundation.md) | Foundation — Scaffold, Auth, Layout | 🟡 In progress | All verticals |
| [2](../../pos-ui/docs/sprints/sprint-2-order-entry.md) | Order Entry — Menu Grid, Cart, Payment | 🔴 Not started | Restaurant, retail |
| [3](../../pos-ui/docs/sprints/sprint-3-tables-shifts.md) | Tables, Floor Plan, Shifts | 🔴 Not started | Restaurant, hotel |
| [4](../../pos-ui/docs/sprints/sprint-4-hotel.md) | Hotel UI — Rooms, Facilities | 🔴 Not started | Hotel, lodge |
| [5](../../pos-ui/docs/sprints/sprint-5-kds.md) | KDS Terminal View | ✅ Complete | Restaurant, bar |
| [6](../../pos-ui/docs/sprints/sprint-6-offline.md) | Offline / PWA | 🔴 Not started | All verticals |
| [7](../../pos-ui/docs/sprints/sprint-7-retail-ui.md) | Retail UI | 🔴 Not started | Supermarket, hardware |
| [8](../../pos-ui/docs/sprints/sprint-8-service-ui.md) | Service Business UI | 🔴 Not started | Salon, clinic, car wash |
| [9](../../pos-ui/docs/sprints/sprint-9-reports-ui.md) | Reports & Analytics UI | 🔴 Not started | All verticals |

---

## Key Documentation

| Document | Purpose |
|----------|---------|
| [ERD](erd.md) | Entity definitions, relationships, and field descriptions |
| [Integrations](integrations.md) | Service-to-service API contracts, NATS events, treasury/inventory wiring |
| [pos-api sprints](sprints/) | Sprint-by-sprint task tracking for the Go API |
| [pos-ui sprints](../../pos-ui/docs/sprints/) | Sprint-by-sprint task tracking for the Next.js PWA |
| [Design reference](../../pos-ui/docs/use-case-designs/hotel-pos-v8.jsx) | Full UX prototype for hotel + restaurant + bar multi-property POS |
