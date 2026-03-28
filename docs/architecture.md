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
  - `rbac.Service` — Role-based access control with granular permissions (126 permissions, 5 system roles). Follows the same pattern as treasury-api and ordering-backend RBAC modules. Repository interface with Ent-backed implementation.
  - `rbac.Repository` — Interface abstraction over POSPermission, POSRoleV2, POSRolePermission, POSUserRoleAssignment entities.
- **Projections**: Maintains a fast, indexed version of the Inventory Product Master.
- **Configuration**: Tax rate, default currency, and order prefix are configurable via env vars (`TAX_RATE_PERCENT`, `DEFAULT_CURRENCY`, `ORDER_PREFIX`).

## Data Authority
- **Primary Owner**: Sales Transactions, Shift Sessions, POS-specific Categories, Modifiers.
- **Consumer**: Inventory Product Master (Master Prices/SKUs).
- **Referencer**: `outlet_id` (Organizational Registry).

## RBAC & Configuration
- **RBAC**: Full role-based access control with `pos.{module}.{action}` permission format. 14 modules, 9 actions per module (126 total). 5 system roles seeded per tenant: `pos_admin`, `store_manager`, `cashier`, `waiter`, `viewer`. Exposed via 7 HTTP endpoints under `/{tenantID}/rbac/`.
- **Rate Limiting**: Database-driven config (`rate_limit_configs`) supporting per-IP, per-tenant, per-user, per-endpoint, and global rate limits with burst multiplier.
- **Service Config**: Key-value configuration (`service_configs`) with platform-level defaults (nil tenant_id) and tenant-specific overrides. Supports typed values (string, int, bool, json, float) and secret masking.

## Offline Resilience
- POS terminals utilize a **Local Cache** (SQLite/Redis) to continue processing sales during internet outages.
- **Reconciliation**: Background workers sync offline transactions once connectivity is restored.
