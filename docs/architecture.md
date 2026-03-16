# POS Service - Architecture Overview

## Design Philosophy
The POS service is designed for **Multi-Domain Flexibility**. It adapts its UI and business workflows based on the `use_case` configured for the Tenant or Outlet.

### Supported Use Cases
- **Hospitality**: Table management, split checks, kitchen display system (KDS).
- **Retail**: Barcode scanning, high-velocity checkout, integrated scales.
- **Quick Service / Kiosk**: Queue-based ordering, self-service UI.

## Layers
- **Core (Domain)**: Deals with Sales Transactions, Shifts, and Catalogs.
- **Projections**: Maintains a fast, indexed version of the Inventory Product Master.
- **Workflow Engine**: Swaps business rules based on the `outlet_type` (e.g., Retail Mode vs. Restaurant Mode).

## Data Authority
- **Primary Owner**: Sales Transactions, Shift Sessions, POS-specific Categories, Modifiers.
- **Consumer**: Inventory Product Master (Master Prices/SKUs).
- **Referencer**: `outlet_id` (Organizational Registry).

## Offline Resilience
- POS terminals utilize a **Local Cache** (SQLite/Redis) to continue processing sales during internet outages.
- **Reconciliation**: Background workers sync offline transactions once connectivity is restored.
