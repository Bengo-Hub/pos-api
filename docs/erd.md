# POS Service – Entity Relationship Overview

**Last updated:** 2026-05-07

**May 7 update:** Hotel module entities added (Room, RoomGuest, RoomFolioItem, Facility, FacilityBooking). KDS entities documented (KDSStation, KDSTicket — schemas existed since March 2026). pos_orders updated with hotel context fields (room_id, room_guest_id, order_subtype). New system roles: receptionist, kitchen, bar.

**March 22 update:** Full RBAC system added alongside existing POSRole/UserPOSRole schemas: POSPermission, POSRoleV2, POSRolePermission (junction), POSUserRoleAssignment, RateLimitConfig, ServiceConfig. RBAC module (`internal/modules/rbac/`) with repository pattern, service layer, and HTTP handler with 7 endpoints. Seed script extended with 126 permissions (14 modules x 9 actions), 5 system roles (pos_admin, store_manager, cashier, waiter, viewer), 6 rate limit configs, and 10 platform-level service configs.

**March 20 update:** Added 30+ HTTP endpoints covering catalog CRUD, table/section management (with floor plan layout fields), tender CRUD, payment recording, cash drawer lifecycle, bar tab management, and promotions. Seed script (`cmd/seed/main.go`) added with outlet (UUID-aligned with ordering-backend/inventory-api), 4 tenders, 3 sections, 12 tables (with VIP/VVIP tags), and 48 catalog items from inventory-api master data. pos-ui pages wired to real API via TanStack Query hooks (`usePOS.ts`).

The POS service delivers a multi-tenant, omni-channel point-of-sale backend supporting cafés/bars, retail, kitchen display, kiosk, and ecommerce scenarios.
Schemas are defined with Ent to ensure type-safe access and migration automation.

> **Conventions**
> - UUID primary keys unless stated.
> - `tenant_id` on all tables for isolation.
> - Timestamps are `TIMESTAMPTZ`.
> - Monetary fields use `NUMERIC(18,2)`; quantities use `NUMERIC(18,6)`.

---

## Tenant & Outlet Structure

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `tenants` | `id`, `slug`, `name`, `status`, `plan_code`, `created_at`, `updated_at` | POS-enabled organisations sharing the canonical `tenant_slug` with ordering-backend, inventory, and logistics services. Entitlements verified against subscription service. |
| `outlets` | `id`, `tenant_id`, `tenant_slug`, `code`, `name`, `channel_type`, `address_json`, `timezone`, `status`, `opened_at`, `closed_at` | Physical/virtual POS outlets (café, kiosk, ecommerce hub) from the shared outlet registry. |
| `outlet_settings` | `outlet_id (PK)`, `receipts_json`, `tax_config_json`, `service_charge_json`, `opening_hours_json`, `metadata`, `updated_at` | Outlet-specific configuration. |
| `pos_devices` | `id`, `tenant_id`, `outlet_id`, `device_code`, `device_type`, `status`, `hardware_fingerprint`, `registered_at`, `last_seen_at`, `metadata` | POS terminals/tablets/kiosks. |
| `pos_device_sessions` | `id`, `tenant_id`, `device_id`, `user_id`, `session_status`, `opened_at`, `closed_at`, `float_amount`, `metadata` | Device session lifecycle with cash float context. |

## Users, Roles & Licensing

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `pos_roles` | `code (PK)`, `name`, `description`, `default_permissions`, `is_system` | Legacy POS-specific roles (cashier, supervisor, manager). Retained for backward compatibility. |
| `user_pos_roles` | `id`, `tenant_id`, `user_id`, `outlet_id`, `role_code`, `assigned_at`, `assigned_by` | Legacy role assignments referencing identities from `auth-service`. Retained for backward compatibility. |
| `license_usage_snapshots` | `id`, `tenant_id`, `snapshot_date`, `active_devices`, `active_users`, `orders_processed`, `api_calls`, `metadata` | Usage metrics for billing/entitlement enforcement. |
| `feature_overrides` | `id`, `tenant_id`, `feature_code`, `override_type`, `value_json`, `effective_from`, `effective_to`, `metadata` | Tenant-specific feature toggles aligned with subscription service. |

## RBAC (Role-Based Access Control)

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `pos_permissions` | `id`, `permission_code` (unique), `name`, `module`, `action`, `resource`, `description`, `created_at` | Granular permissions in `pos.{module}.{action}` format. 14 modules x 9 actions = 126 permissions. |
| `pos_role_v2s` | `id`, `tenant_id`, `role_code`, `name`, `description`, `is_system_role`, `created_at`, `updated_at` | RBAC roles per tenant. System roles: pos_admin, store_manager, cashier, waiter, viewer. |
| `pos_role_permissions` | `role_id`, `permission_id` | Junction table linking roles to permissions. Unique on (role_id, permission_id). |
| `pos_user_role_assignments` | `id`, `tenant_id`, `user_id`, `role_id`, `assigned_by`, `assigned_at`, `expires_at` | User-to-role assignments with optional expiry. Unique on (tenant_id, user_id, role_id). |

## Rate Limiting & Service Configuration

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `rate_limit_configs` | `id`, `service_name`, `key_type`, `endpoint_pattern`, `requests_per_window`, `window_seconds`, `burst_multiplier`, `is_active`, `description` | Database-driven rate limit configuration. Key types: ip, tenant, user, endpoint, global. |
| `service_configs` | `id`, `tenant_id` (nullable), `config_key`, `config_value`, `config_type`, `description`, `is_secret` | Service-level key-value configuration. Nil tenant_id = platform default; set = tenant override. |

## Catalog & Pricing (Sync with Inventory/Food Delivery)

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `catalog_items` | `id`, `tenant_id`, `external_item_id`, `source_service`, `name`, `category`, `barcode`, `base_price`, `tax_code`, `modifier_schema`, `status`, `metadata`, `synced_at` | Mirror of master catalog from inventory/food delivery. |
| `price_books` | `id`, `tenant_id`, `name`, `channel_scope`, `effective_from`, `effective_to`, `status`, `metadata` | Different pricing sets (happy hour, wholesale). |
| `price_book_items` | `id`, `price_book_id`, `catalog_item_id`, `price`, `currency`, `discount_type`, `discount_value`, `metadata` | Price overrides per catalog item. |
| `modifier_groups` | `id`, `tenant_id`, `name`, `required`, `min_select`, `max_select`, `metadata` | Modifiers (toppings, sides). |
| `modifiers` | `id`, `modifier_group_id`, `catalog_item_id`, `label`, `price_delta`, `metadata` | Options within modifier groups. |

## Orders & Ticketing

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `pos_orders` | `id`, `tenant_id`, `outlet_id`, `device_id`, `order_number`, `channel`, `status`, `order_type`, `tab_name`, `customer_id`, `subtotal`, `discount_total`, `tax_total`, `service_charge_total`, `tip_total`, `total_amount`, `paid_amount`, `balance_amount`, `currency`, `opened_at`, `closed_at`, `metadata` | POS order header. |
| `pos_order_lines` | `id`, `pos_order_id`, `catalog_item_id`, `variant_id`, `name_snapshot`, `quantity`, `unit_price`, `discount_amount`, `tax_amount`, `notes`, `metadata` | Line items. |
| `pos_line_modifiers` | `id`, `order_line_id`, `modifier_id`, `label_snapshot`, `price_delta`, `metadata` | Applied modifiers. |
| `pos_order_events` | `id`, `pos_order_id`, `event_type`, `payload`, `actor_user_id`, `occurred_at` | Status changes, voids, discounts, reopenings. |
| `sections` | `id`, `tenant_id`, `outlet_id`, `name`, `slug`, `description`, `floor_number`, `sort_order`, `is_active`, `section_type` (main_hall/outdoor/private_room/bar/vip/vvip/rooftop), `metadata`, `created_at`, `updated_at` | Floor plan sections that organize tables into logical areas. Added March 2026. |
| `tables` | `id`, `tenant_id`, `outlet_id`, `section_id` (FK optional), `name`, `capacity`, `status`, `table_type` (standard/booth/bar_seat/counter/vip/vvip), `x_position`, `y_position` (for floor plan rendering), `tags` (JSON: VIP, Window, etc.), `metadata`, `created_at`, `updated_at` | Dining area table definitions with spatial layout support. Updated March 2026 with section FK, position, type, tags. |
| `table_assignments` | `id`, `table_id`, `pos_order_id`, `assigned_at`, `released_at`, `metadata` | Table ↔ order linkage. |
| `bar_tabs` | `id`, `tenant_id`, `outlet_id`, `tab_code`, `customer_name`, `limit_amount`, `opened_by`, `opened_at`, `closed_at`, `status`, `metadata` | Bar/KTV tab tracking. |
| `bar_tab_events` | `id`, `bar_tab_id`, `event_type`, `payload`, `occurred_at`, `actor_user_id` | Tab lifecycle. |

## Tendering & Cash Management

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `tenders` | `id`, `tenant_id`, `name`, `tender_type`, `provider_code`, `is_active`, `metadata` | Accepted payment types (cash, card, mobile money, loyalty). |
| `pos_payments` | `id`, `pos_order_id`, `tender_id`, `amount`, `currency`, `tip_amount`, `surcharge_amount`, `payment_status`, `provider_reference`, `processed_at`, `metadata` | Payment records routed through `treasury-app`. |
| `cash_drawers` | `id`, `tenant_id`, `outlet_id`, `device_id`, `opening_user_id`, `closing_user_id`, `opening_float`, `closing_amount`, `variance_amount`, `status`, `opened_at`, `closed_at`, `metadata` | Drawer lifecycle. |
| `cash_drawer_events` | `id`, `cash_drawer_id`, `event_type`, `amount`, `performed_by`, `performed_at`, `notes`, `metadata` | Skims, drops, shortages, audits. |
| `pos_refunds` | `id`, `tenant_id`, `pos_order_id`, `payment_id`, `amount`, `reason`, `initiated_by`, `initiated_at`, `status`, `metadata` | Refund transactions. |

## Promotions, Loyalty & Gift Cards

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `promotions` | `id`, `tenant_id`, `code`, `name`, `description`, `promotion_type`, `value`, `start_at`, `end_at`, `usage_limit`, `status`, `metadata` | Local POS promotions. |
| `promotion_rules` | `id`, `promotion_id`, `condition_type`, `condition_json`, `reward_type`, `reward_json`, `metadata` | Rule engine details. |
| `promotion_applications` | `id`, `promotion_id`, `pos_order_id`, `applied_amount`, `applied_at`, `metadata` | Audit record. |
| `gift_cards` | `id`, `tenant_id`, `card_number`, `pin_hash`, `status`, `balance_amount`, `issued_at`, `expires_at`, `metadata` | Stored-value cards (optional integration with treasury). |
| `gift_card_transactions` | `id`, `gift_card_id`, `pos_order_id`, `amount`, `transaction_type`, `occurred_at`, `metadata` | Gift card ledger. |

## Inventory Touchpoints (via Inventory Service)

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `stock_consumption_events` | `id`, `tenant_id`, `tenant_slug`, `pos_order_id`, `item_id`, `warehouse_id`, `quantity`, `uom_code`, `source`, `created_at`, `metadata` | Events emitted to `inventory-service` for decrementing stock/BOM consumption using canonical tenant/outlet/item identifiers. |
| `stock_alert_subscriptions` | `id`, `tenant_id`, `outlet_id`, `item_id`, `threshold`, `channel`, `metadata` | User-configurable alerts; listens to inventory events. |
| `inventory_snapshots` | `id`, `tenant_id`, `outlet_id`, `item_id`, `on_hand`, `available`, `snapshot_at`, `source_service` | Read-only cached view of inventory for UI performance (not canonical). |

## Kitchen Display System (KDS)

Schemas exist since March 2026. HTTP endpoints added in Sprint 4.

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `kds_stations` | `id`, `tenant_id`, `outlet_id`, `name`, `category_filter` (JSON string array), `sort_order`, `is_active`, `created_at`, `updated_at` | KDS display stations — kitchen, bar, grill, etc. Each station receives tickets for items matching its category filter. |
| `kds_tickets` | `id`, `tenant_id`, `station_id` (FK → kds_stations), `order_id`, `order_number`, `status` (pending\|in_progress\|ready\|served\|voided), `items` (JSON: `[{line_id, sku, name, qty, kds_status}]`), `received_at`, `started_at`, `completed_at`, `priority` | One ticket per station per order. Created when order transitions to `open`. Items track individual item-level status. |

## Hotel Module (Sprint 3 — Not Yet Started)

Ent schemas to be added. HTTP endpoints under `/{tenant}/hotel/`.

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `rooms` | `id`, `tenant_id`, `outlet_id`, `room_number`, `name`, `room_type` (standard\|deluxe\|suite\|presidential\|other), `floor`, `rate_per_night`, `currency`, `status` (available\|occupied\|cleaning\|maintenance\|reserved\|checkout), `is_active`, `metadata`, `created_at`, `updated_at` | Hotel room inventory. Unique on (tenant_id, room_number). |
| `room_guests` | `id`, `tenant_id`, `room_id` (FK → rooms), `guest_name`, `phone`, `id_number`, `check_in_date`, `nights`, `check_out_date`, `total_room_charge`, `status` (active\|checked_out), `checked_in_by`, `checked_out_by`, `checked_in_at`, `checked_out_at`, `metadata`, `created_at`, `updated_at` | Active and historical guest stays. |
| `room_folio_items` | `id`, `tenant_id`, `room_id` (FK → rooms), `room_guest_id` (FK → room_guests), `description`, `amount`, `currency`, `charge_type` (room_charge\|food\|laundry\|minibar\|room_service\|other), `pos_order_id` (nullable — linked POS order), `created_at` (Immutable), `created_by`, `metadata` | Per-stay charge history. Room charge added at check-in; F&B/minibar added as POS orders complete. |
| `facilities` | `id`, `tenant_id`, `outlet_id`, `name`, `facility_type` (pool\|gym\|conference\|spa\|kids_area\|other), `capacity`, `rate_per_session`, `currency`, `opening_time`, `closing_time`, `status` (available\|occupied\|maintenance\|closed), `is_active`, `metadata`, `created_at`, `updated_at` | Hotel facilities (pool, gym, spa, conference). |
| `facility_bookings` | `id`, `tenant_id`, `facility_id` (FK → facilities), `room_guest_id` (nullable), `guest_name`, `phone`, `session_date`, `start_time`, `end_time`, `guests_count`, `amount`, `currency`, `status` (confirmed\|cancelled\|completed), `booked_by`, `notes`, `metadata`, `created_at` | Facility session bookings. |

**pos_orders hotel context fields (to add):**
- `room_id` — UUID nullable, links room-service orders to a room
- `room_guest_id` — UUID nullable, links room-service orders to a guest stay
- `order_subtype` — enum(dine_in\|takeaway\|room_service\|delivery\|bar_tab) default dine_in

**New RBAC permissions (Sprint 3):** `pos.hotel.view`, `pos.hotel.change`, `pos.hotel.manage`  
**New RBAC permissions (Sprint 4):** `pos.kds.view`, `pos.kds.change`, `pos.kds.manage`  
**New system roles:** `receptionist` (hotel.view + hotel.change), `kitchen` (kds.view + kds.change), `bar` (kds.view + kds.change)

## Ecommerce & Omnichannel
| Table | Key Columns | Description |
|-------|-------------|-------------|
| `channel_integrations` | `id`, `tenant_id`, `channel_type`, `config_json`, `status`, `last_sync_at`, `metadata` | Ecommerce, marketplace, and kiosk connectors. |
| `channel_sync_jobs` | `id`, `channel_integration_id`, `sync_type`, `status`, `started_at`, `finished_at`, `items_processed`, `error_message` | Audit log of sync operations. |
| `order_links` | `id`, `tenant_id`, `pos_order_id`, `external_order_id`, `source_service`, `synced_at`, `sync_status`, `metadata` | Link to ordering-backend or other channels. |

## Reporting & Compliance
| Table | Key Columns | Description |
|-------|-------------|-------------|
| `till_reports` | `id`, `tenant_id`, `outlet_id`, `device_id`, `report_date`, `opening_float`, `closing_amount`, `cash_variance`, `tender_breakdown_json`, `generated_by`, `generated_at` | End-of-day cash/tender summary. |
| `sales_reports` | `id`, `tenant_id`, `report_date`, `outlet_id`, `channel`, `gross_sales`, `net_sales`, `tax_total`, `refund_total`, `discount_total`, `metadata` | Daily sales metrics. |
| `audit_logs` | `id`, `tenant_id`, `user_id`, `resource_type`, `resource_id`, `action`, `payload`, `ip_address`, `user_agent`, `occurred_at` | Compliance log (voids, discounts, drawer events). |
| `regulatory_exports` | `id`, `tenant_id`, `export_type`, `period_start`, `period_end`, `status`, `file_url`, `requested_by`, `requested_at`, `completed_at`, `metadata` | Fiscal authority exports / ETR submissions. |

## Integrations & Eventing
| Table | Key Columns | Description |
|-------|-------------|-------------|
| `integration_settings` | `id`, `tenant_id`, `service_code`, `config_json`, `status`, `last_sync_at`, `metadata` | Config for treasury, inventory, ordering-backend, notifications. |
| `webhook_subscriptions` | `id`, `tenant_id`, `event_key`, `target_url`, `secret`, `status`, `last_delivery_status`, `retry_count` | Outbound webhooks (order complete, drawer closed, stock alert). |
| `outbox_events` | `id`, `tenant_id`, `aggregate_type`, `aggregate_id`, `event_type`, `payload`, `status`, `attempts`, `last_attempt_at`, `created_at` | Reliable event dispatcher for NATS/Kafka. |
| `sync_failures` | `id`, `tenant_id`, `integration_code`, `error_code`, `payload`, `occurred_at`, `resolved_at`, `metadata` | Error tracking to surface in admin console. |
| `tenant_sync_events` | `id`, `tenant_id`, `tenant_slug`, `source_service`, `payload`, `synced_at`, `status` | Tracks inbound tenant/outlet discovery requests (e.g., from auth or ordering-backend) before devices/orders are created. |

## Relationships & External Services

**Entity Ownership**: This service owns POS-specific entities. It references (but does not own) entities from other services:
- **Users**: References `user_id` from `auth-service` (never stores user accounts)
- **Tenants/Outlets**: References `tenant_id`, `tenant_slug`, `outlet_id` from `auth-service` registry
- **Catalog Items**: References `item_id`/`sku` from `inventory-service` (stores `catalog_items` as read-only cache)
- **Inventory Balances**: Queries `inventory-service` APIs (stores `inventory_snapshots` as read-only cache)
- **Payments**: Calls `treasury-app` APIs (stores `pos_payments` as references, not full transactions)
- **Riders/Drivers**: Queries `logistics-service` APIs (never stores rider data)

- `pos_orders` connect to `order_links` for references in ordering-backend; no duplication of order state there because both sides share the tenant/outlet registry, hydrated via discovery webhooks when a tenant/outlet first appears.
- Tenant/outlet discovery callbacks from auth/ordering-backend ensure this service can provision outlets/devices on demand without manual synchronisation or polling.
- `user_pos_roles.user_id` references identities from `auth-service` (token claims drive UI permissions).
- `stock_consumption_events` push to `inventory-service`; canonical stock levels remain in inventory.
- `pos_payments` integrate with `treasury-app` for payment authorization, settlement, and refunds.
- Notifications (drawer discrepancies, stock alerts) route through `notifications-app`.
- `channel_integrations` coordinate with ecommerce storefronts and logistics service for click-and-collect workflows.
- See **shared-docs/CROSS-SERVICE-DATA-OWNERSHIP.md** for the canonical entity ownership matrix and integration patterns.

## Seed & Defaults
- Default roles: `cashier`, `supervisor`, `manager`, `inventory_clerk` (legacy POSRole).
- RBAC system roles: `pos_admin` (all permissions), `store_manager`, `cashier`, `waiter`, `viewer` with granular permission assignments.
- 126 RBAC permissions across 14 modules (orders, payments, catalog, outlets, devices, sessions, cash_drawers, tables, gift_cards, price_books, modifiers, channels, config, users) and 9 actions (add, view, view_own, change, change_own, delete, delete_own, manage, manage_own).
- 6 rate limit configs (global, tenant, IP, user, order creation, payment recording).
- 10 platform-level service configs (currency, tax rate, session timeout, etc.).
- Demo outlets (flagship cafe, express kiosk) seeded for QA.
- Standard tenders (cash, card, Mpesa, loyalty) and example price book configured for demonstrations.

---
Regenerate this ERD whenever Ent schemas evolve. Always run `go generate ./internal/ent` before committing schema changes and update integration docs accordingly.

