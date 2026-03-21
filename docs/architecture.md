# POS Service - Architecture Overview

## Design Philosophy
The POS service is designed for **Multi-Domain Flexibility**. It adapts its UI and business workflows based on the `use_case` configured for the Tenant or Outlet.

### Supported Use Cases
- **Hospitality**: Table management, split checks, kitchen display system (KDS).
- **Retail**: Barcode scanning, high-velocity checkout, integrated scales.
- **Quick Service / Kiosk**: Queue-based ordering, self-service UI.

## Layers
- **Core (Domain)**: Deals with Sales Transactions, Shifts, and Catalogs.
- **Service Layer** (`internal/modules/`):
  - `orders.Service` — Order creation with tax/discount calculation, order number generation, status state machine (draft → open → completed/cancelled/voided → refunded).
  - `payments.Service` — Payment recording with proper state transitions; auto-completes order only when fully paid.
  - `promotions.Service` — Promo code validation with actual discount calculation (percentage/fixed with max cap).
- **Projections**: Maintains a fast, indexed version of the Inventory Product Master.
- **Configuration**: Tax rate, default currency, and order prefix are configurable via env vars (`TAX_RATE_PERCENT`, `DEFAULT_CURRENCY`, `ORDER_PREFIX`).

## Data Authority
- **Primary Owner**: Sales Transactions, Shift Sessions, POS-specific Categories, Modifiers.
- **Consumer**: Inventory Product Master (Master Prices/SKUs).
- **Referencer**: `outlet_id` (Organizational Registry).

## Offline Resilience
- POS terminals utilize a **Local Cache** (SQLite/Redis) to continue processing sales during internet outages.
- **Reconciliation**: Background workers sync offline transactions once connectivity is restored.
