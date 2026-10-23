# POS - Sprint: Device Trail, Offline Attribution and Shift Cash-up

**Created:** 2026-10-23. **Driver:** an Urban Loft investigation into order 002384, which could not
say which phone or PC it came from, and showed it under a manager who was reportedly off that day.
**Status:** Shipped in pos-api + pos-ui, pending deploy.

## Why

Three gaps came out of that investigation.

1. **No device identity.** Every browser at an outlet reuses one `web_terminal` device row, orders
   kept a zero `device_id`, and nothing stored the user agent or IP. PIN sign-ins were not
   audited. Ingress logs roll over within days, so after that there was no way to tell devices
   apart.
2. **Offline sales credited to the wrong person.** The offline queue did not store who rang a
   sale up, so a sale was credited to whoever was logged in when the browser came back online,
   possibly hours later and under a different login.
3. **Shift cash-up never worked.** Shift figures filtered orders by `device_id` (zero on almost
   every order) and matched cash by `tender_id` (zero on every payment), so expected cash always
   equalled the opening float. The shift report counted every completed order ever made on the
   shared device, with no time bound.

## Client trail

`internal/platform/clientinfo` builds an `Info` per request: the client id and device name pos-ui
sends as `X-Client-Id` / `X-Device-Name` (URI-encoded), the user agent, a parsed device kind
(desktop, mobile, tablet), OS and browser, and the IP from `ratelimit.ClientIP` (behind
`TrustedRealIP`). Both headers are added to the service's CORS allow list.

`audit.TrailHook` is one ent create hook registered in `app.New`. It stamps the request's Info on:

| Row | Where |
|-----|-------|
| `pos_orders` | `metadata.client` |
| `pos_payments` | `payment_data.client`, plus `payment_data.collected_by` (the logged-in user) |
| `pos_device_sessions` | `metadata.opened_from`; `shifts.CloseSession` adds `closed_from` |
| `audit_logs` | `metadata.client` (new nullable `metadata` column) |

The server value always overwrites a client-supplied `client` key. Rows written by workers and
event consumers have no request and are left alone.

PIN sign-ins now write `auth.pin_login`, and wrong PINs write `auth.pin_failed` (a PIN that matched
nobody is recorded against the zero user id). Each sign-in also records the browser on the outlet
device's `metadata.clients` under a row lock (`touchOutletDeviceClient`), keeping the 50 most
recent. New orders fall back to the outlet's `web_terminal` id instead of the zero UUID.

## Offline attribution

pos-ui stores `rung_up_by_user_id`, `rung_up_by_name` and a device snapshot with each queued sale
(and `collected_by_user_id` with each queued payment), and replays them on sync as `offline` and
`offlineCollectedBy`. pos-api credits the sale to the rung-up person only when they are an active
staff member of the tenant, and otherwise falls back to the syncing login. `metadata.offline`
keeps both people, the device it was rung up on and the sync time, and an `order.offline_sync`
audit entry is written. A queued payment's collector is honoured only for an offline order
credited to that same person. Queued sales also now keep their order type and customer.

## Shift scope (`internal/modules/shifts/summary.go`)

One scope feeds the close/auto-close variance, the live summary, the history list, the shift
report and its PDF:

- **orders:** the shift user's orders rung up inside the shift window (`offline_created_at` for
  synced offline sales, else `created_at`).
- **payments:** completed payments taken in the window and recorded by the shift user
  (`collected_by`); older rows without it fall back to the order's cashier.
- **cash refunds:** completed cash-channel returns the user completed in the window.

Expected cash = opening float + cash taken - cash refunds. Tender breakdowns everywhere (shift and
daily reset PDFs included) group by `payment_data.method`, using a configured tender's name when a
payment references one. Before this, they collapsed into one "Unknown" row.

**Behaviour change:** from the deploy, cash-up variance reflects real takings. Shifts where staff
counted 0 at close will now show the shortfall.

## Loss Prevention page

`GET /reports/audit-logs` adds filters `client`, `ip`, `order` (order number, covering the order's
lines too) and comma-separated `action`, and returns `client`, `entity_label` and `order_id` per
row. `GET /reports/exceptions` groups in SQL over the requested range (default: last 30 days),
covers the actions that actually occur (deletes, reversals, comps, price edits, failed PINs,
offline sales synced under a different login), and lists the devices and IPs each person signed
in from.

## Indexes (migration `20261023090000_client_trail_and_shift_indexes.sql`)

`pos_payments(order_id)`, `pos_payments(occurred_at)`, `pos_device_sessions(tenant_id, user_id,
session_status)`, `(tenant_id, device_id, session_status)`, `(tenant_id, opened_at)`,
`pos_orders(tenant_id, user_id, created_at)`, `pos_refunds(order_id)`. Before this, payments and
sessions had only primary keys, so every payments-by-order load scanned the table.

## Also in this sprint

- Returns list `q` filter (return number, or the sale's invoice number, customer name or phone),
  and order search `q` (invoice number or customer) used by the Initiate Return picker.
- Order detail shows an Origin card (device, IP, offline origin), and its payments panel now reads
  `payment_data.method` / `occurred_at` (it showed "—" for every payment).

## Follow-ups

- Exchange returns with a cash leftover are not yet netted out of expected cash.
- `ILIKE '%term%'` customer search scans the tenant's orders; add a `pg_trgm` index if it gets slow.
- The `clientinfo` parser could move into `httpware` if other services want the same trail.
