-- backfill_clear_stale_kds_tickets.sql
--
-- One-off cleanup: force-serve KDS tickets still sitting in pending/in_progress/ready whose
-- parent order is already 'completed' (fully paid/settled). Before orders.Service.
-- AutoClearKDSTicketsForOrder existed, an order could settle (quick-service counter sale, a
-- printer-only kitchen station, or a ticket the kitchen never manually bumped) without its
-- tickets ever being marked served — leaving food that had already been served and paid for
-- stuck on the live kitchen/bar board indefinitely.
--
-- Matches the new auto-clear logic exactly (see AutoClearKDSTicketsForOrder in
-- internal/modules/orders/service.go), so this is the retroactive counterpart for tickets that
-- predate the fix. Safe to re-run (idempotent): it only ever matches tickets in a non-terminal
-- state on an already-completed order.
--
-- Run against the pos DB. PREVIEW first, then apply.

-- ── Preview (per tenant) ────────────────────────────────────────────────────────
SELECT t.tenant_id, count(*) AS stale_tickets
FROM kds_tickets t
JOIN pos_orders o ON o.id = t.order_id
WHERE t.status IN ('pending', 'in_progress', 'ready')
  AND o.status = 'completed'
GROUP BY t.tenant_id
ORDER BY stale_tickets DESC;

-- ── Apply ───────────────────────────────────────────────────────────────────────
BEGIN;

UPDATE kds_tickets t
SET status = 'served', completed_at = now()
FROM pos_orders o
WHERE o.id = t.order_id
  AND t.status IN ('pending', 'in_progress', 'ready')
  AND o.status = 'completed';

COMMIT;

-- ── Verify (should return 0) ──────────────────────────────────────────────────
SELECT count(*)
FROM kds_tickets t
JOIN pos_orders o ON o.id = t.order_id
WHERE t.status IN ('pending', 'in_progress', 'ready')
  AND o.status = 'completed';
