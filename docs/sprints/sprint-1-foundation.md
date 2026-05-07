# Sprint 1: Foundation — pos-api

**Status:** ✅ Complete  
**Period:** January–February 2026  
**Goal:** Core infrastructure, authentication, RBAC, devices, sessions, seeding

---

## Deliverables

### Authentication & Identity
- [x] SSO JWT validation via `shared-auth-client` (JWKS/RS256)
- [x] API key validation for S2S calls
- [x] Dual-auth middleware (`RequireAuth`) applied to all protected routes
- [x] JIT user provisioning from JWT claims on first request
- [x] JIT tenant syncing from auth NATS events (`auth.tenant.created`, `auth.tenant.updated`)
- [x] `GET /{tenant}/auth/me` — returns JWT claims + POS RBAC roles/permissions
- [x] Subscription enforcement (mutations-only): GET passes through; POST/PUT/PATCH/DELETE require active subscription
- [x] Platform owner bypass (`is_platform_owner` + `superuser` role)

### RBAC
- [x] `POSPermission` entity — 126 permissions across 14 modules × 9 actions (`pos.{module}.{action}`)
- [x] `POSRoleV2` entity — system roles: `pos_admin`, `store_manager`, `cashier`, `waiter`, `viewer`
- [x] `POSRolePermission` junction
- [x] `POSUserRoleAssignment` entity (user ↔ role with expiry)
- [x] `rbac.Service` and `rbac.Repository` (repository pattern)
- [x] 7 RBAC HTTP endpoints under `/{tenant}/rbac/`
- [x] Seed: 126 permissions, 5 system roles, role-permission assignments

### Devices & Sessions
- [x] `POSDevice` entity (device_code, device_type, hardware_fingerprint)
- [x] `POSDeviceSession` entity (shift lifecycle, float_amount, opened_at, closed_at)
- [x] `Outlet` + `OutletSetting` entities
- [x] `Tenant` entity with tenant slug

### Infrastructure
- [x] PostgreSQL (pgx/v5) with versioned Atlas migrations
- [x] Redis (`Bengo-Hub/cache v0.2.0`) for rate-limit counters, auth/me cache
- [x] NATS (`shared-events v0.2.0`) — outbox publisher running
- [x] Rate limit configs DB-driven (`rate_limit_configs` table)
- [x] Service configs DB-driven (`service_configs` table)
- [x] Swagger (`/v1/docs/`) at service root
- [x] `GET /healthz` + `GET /readyz` + `GET /metrics` (Prometheus)
- [x] Seed script (`cmd/seed/main.go`) — outlet, tenders, sections, tables, catalog items, RBAC

### Events Published
- `pos.tenant.synced` — tenant/outlet creation confirmed

### Events Consumed
- `auth.tenant.created` — provision tenant record
- `auth.tenant.updated` — update tenant slug/status
- `auth.outlet.created` — provision outlet
- `subscriptions.subscription.activated` — update plan entitlements
- `subscriptions.subscription.cancelled` — enforce subscription limits

---

## Pending / Carry-forward
- [ ] `pos_device_sessions` — device-specific shift endpoints (`/sessions/open`, `/sessions/close`) not yet wired in HTTP handler
- [ ] Outlet selector at login (post-MVP: cashier selects outlet per terminal)
