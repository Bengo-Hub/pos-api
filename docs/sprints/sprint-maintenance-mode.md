# POS - Sprint: Tenant Maintenance Window ("Repair Mode")

**Created:** 2026-09-17 - **Driver:** live user request, implemented same day.
**Status:** Shipped (pos-api + pos-ui), pending deploy + activation.

Product/UI name is "Repair Mode" (the user's own term). Named `maintenance` internally in code to
avoid confusion with the pre-existing, unrelated `RepairJob` device/job-card module documented in
`sprint-repair-module.md` - same word, different feature, same service.

## What it does

A platform-owner-only, per-tenant, scheduled lockout window. While `now` is inside
`[maintenance_starts_at, maintenance_ends_at]` for a tenant, every request to that tenant's POS
from a non-platform-owner is blocked with a structured `tenant_under_repair` response, and pos-ui
shows a full-screen "Under Maintenance" overlay that blocks all interaction. Access resumes
automatically once `ends_at` passes - there is no separate on/off flag to remember to flip back.

Deliberately scoped to pos-service only (pos-api + pos-ui), not the whole platform - matches
where the user's request was framed ("turn the pos access off") and where the existing repair-job
module already lives. Not extended to inventory-ui/treasury-ui/ordering-frontend.

Tenant-scoped by construction: the window lives on pos-api's own local `Tenant` row (one tenant
at a time), activated via a per-tenant endpoint - there is no fleet-wide "maintenance every
tenant" switch.

## Schema (pos-api local `Tenant`, Ent + Atlas)

Migration `20260917064702_add_tenant_maintenance_window.sql`:
- `maintenance_starts_at` (nullable timestamptz)
- `maintenance_ends_at` (nullable timestamptz)
- `maintenance_reason` (nullable text, shown on the banner)
- `maintenance_activated_by` (nullable text, the platform owner's email who scheduled it)

Both `starts_at`/`ends_at` must be set for the window to ever be enforced (see
`middleware.UnderMaintenance`) - a half-set window fails open, not closed, so a scheduling
mistake never accidentally locks a tenant out forever.

## Backend

- `internal/http/middleware/maintenance.go` - `RequireNotUnderMaintenance`, mounted in
  `router.go`'s protected route chain (same axis group as `SubscriptionGate`/
  `RequireServiceAccess`/`RequireSupportFeeCurrentForMutations`). Bypasses on
  `claims.IsPlatformOwner` only - a tenant superuser/admin is NOT exempt, matching the user's
  explicit requirement that no tenant role can access the system in repair mode. Short (15s)
  in-process cache so activation/expiry takes effect promptly; `InvalidateMaintenanceCache` makes
  the admin toggle's own write visible immediately instead of waiting out the TTL.
- `internal/http/handlers/pin_auth.go` - `checkMaintenanceGate`, called first in both `Login` and
  `IdentifyByPIN`, before any staff/PIN lookup. During an active window, ordinary staff PINs are
  rejected with the same `tenant_under_repair` error instead of "invalid credentials"; a separate
  platform override PIN (bcrypt-hashed, config `PLATFORM_REPAIR_PIN_HASH`, never a literal in
  source) mints a short-lived (1h) platform-owner terminal session instead, so a platform
  engineer can get into the till itself during on-site repair work.
- `internal/http/handlers/maintenance_window.go` - platform-owner-only
  `GET/POST/DELETE /api/v1/maintenance-window/{tenant_id}` to read/schedule/cancel a tenant's
  window. Cancel clears the window immediately regardless of where `now` sits inside it.

## Frontend (pos-ui)

- `store/maintenance.ts` + `components/pos/maintenance-overlay.tsx` - global, non-dismissable
  full-screen overlay (z-[100], above every other modal in the app), shown the instant any
  request comes back `tenant_under_repair` (wired via a new `apiClient.setOnTenantUnderRepair`
  callback in `providers/auth-provider.tsx`) - covers both an already-open session hitting the
  block mid-use and the PIN-login screen's own login attempt (same `apiClient` instance, same
  interceptor).
- `components/pos/sync-monitor/maintenance-window-tab.tsx` - platform-owner tab on the existing
  Sync Monitor page (next to Txn Reversals), reusing `usePlatformTenants` for the fleet-wide
  tenant picker. Schedules/cancels a window against the real endpoint above; the UI never
  enforces anything itself, it only reads/writes the window.

## Known limitations / deliberately out of scope

- Platform-wide (not per-activation) override PIN - a single value, by explicit user decision,
  accepted after flagging the weak-PIN risk (this codebase already has a documented incident
  about exactly this class of bug - see `pin_auth.go`'s `weakPINs` block-list comment).
- pos-service only, not a whole-platform kill switch.
- No historical audit log beyond the single `maintenance_activated_by`/`maintenance_reason`
  fields on the current/last window - scheduling a new window overwrites the previous record's
  reason/activated_by, there's no separate history table.
