-- backfill_null_fake_etims_numbers.sql
--
-- One-off cleanup: clear FAKE eTIMS invoice numbers that treasury-api stamped onto POS
-- orders while its KRA client ran unconfigured and fabricated receipt numbers (the value
-- is a 10-digit Unix timestamp, e.g. 1783757336, not a real KRA receipt number). These
-- orders were never fiscalized. See the eTIMS gating fix in treasury-api.
--
-- Safe to re-run (idempotent): matches only the 10-digit Unix-timestamp pattern. Real KRA
-- receipt numbers (short, non-10-digit sequences) are left untouched.
--
-- Run against the pos DB. PREVIEW first, then apply.

-- ── Preview ─────────────────────────────────────────────────────────────────────
SELECT count(*) AS fake_stamped_orders
FROM pos_orders
WHERE etims_invoice_number ~ '^[0-9]{10}$';

-- ── Apply ───────────────────────────────────────────────────────────────────────
BEGIN;

UPDATE pos_orders
SET etims_invoice_number = NULL,
    etims_qr_code_url    = NULL
WHERE etims_invoice_number ~ '^[0-9]{10}$';

COMMIT;
