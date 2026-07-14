# POS Printing Architecture

Updated: 2026-07-06 (background print queue — AccuPOS model)

## Overview

Printing has two cooperating layers:

1. **Server print queue (preferred)** — pos-api renders ESC/POS server-side and enqueues
   `print_jobs`; the on-site **Local Print Agent** (paired per outlet) polls, claims and prints
   them to network printers (raw TCP 9100) and locally-installed USB/OS printers (Windows spooler
   RAW / CUPS `lp -o raw`). The till UI enqueues and moves on — printing never blocks the waiter
   logout and never opens a browser dialog.
2. **Client transports (fallback)** — when no agent is polling, pos-ui prints via the loopback
   agent relay (127.0.0.1:9330 `/print`, network by ip:port or local by name), else QZ Tray, else —
   only for *explicit* manual actions — a browser print window.

## Server components (pos-api)

- `internal/modules/printing/escpos.go` — `BuildReceipt` (customer/kitchen_ticket/waiter_copy/void),
  `BuildTestTicket`.
- `internal/modules/printing/orderdata.go` — `OrderReceiptData` / `StationTicketData`: the single
  order→ReceiptData mappers (shared by `/print` handler and the queue — never duplicate).
- `internal/modules/printing/profiles.go` — `ProfilesFromRaw` (single decoder for
  `OutletSetting.printer_profiles`), `ResolveBillProfile` (customer → waiter → any real),
  `ProfileForStation`, `HasRealPrinter` (mirrors pos-ui `printer-stations.ts`).
- `internal/modules/printing/queue.go` — `Queue`: `Enqueue` (dedupe key per (tenant, key)),
  `ClaimNext` (long-poll; lease `claim_expires_at` 60s; job TTL 15 min; ≤3 attempts), `Ack`,
  `AgentOnline` (agent polled within 90s).

  **Performance contract (idle polling must be near-free):** the 1s in-poll loop's common case is
  a single index-backed `EXISTS` — no transaction, no locks, no writes. The locking claim
  transaction (Postgres `FOR UPDATE SKIP LOCKED`) opens only when the probe found a claimable job.
  The `claimable` predicate is the correctness source of truth: `queued OR (claimed AND lease
  expired)` (dead-agent jobs reclaim instantly, no sweep dependency) within the TTL window and
  attempt cap. Status sweeps (expired-lease → queued, over-TTL → expired) are pure hygiene for
  reporting and run at most once per 15s per process (atomic CAS gate). Enqueue points check
  `AgentOnline` FIRST, so unpaired outlets pay exactly one `EXISTS` per order/payment.
- Ent entities: `PrintJob` (`print_jobs`), `PrintAgent` (`print_agents`, SHA-256 `key_hash`).
  Migration `20260706183709_add_print_jobs_and_agents.sql`. Both are in the tenant purge plan.

### Enqueue points

- **Order create** (`orders/service.go` → `print_enqueue.go`): for hospitality orders, when a
  paired agent is ONLINE — `auto_print_kitchen` → per-station kitchen/bar tickets (same
  `routeLinesToStations` routing as KDS tickets), `auto_print_order` → customer bill via
  `ResolveBillProfile`. Dedupe `orderID:jobType:stationID` / `orderID:bill:profileID`.
- **Payment finalization** (`payments/print_enqueue.go`): full payment + `auto_print_order` +
  agent online → final receipt (tender names joined as the payment method). Dedupe
  `orderID:receipt:profileID`.
- **Explicit** — `POST /{tenant}/pos/printing/jobs` `{job_type: bill|receipt|test|drawer, order_id?,
  outlet_id?, profile_id?}`: Print Bill / Print Receipt buttons and Test print. Enqueues nothing and
  returns `agent_online:false` when no agent is polling (caller falls back to client transports).

### Agent pairing + polling API

- `POST /{tenant}/pos/printing/agents` (JWT, `pos.config.change`) → `{id, key}` — plaintext key
  returned ONCE. `GET` lists (+`agent_online`), `DELETE /{agentID}` revokes.
- Agent side (X-Agent-Key auth, public routes): `GET /api/v1/pos/printing/agent/jobs?wait=25`
  (long-poll claim, bumps `last_seen_at`), `POST /api/v1/pos/printing/agent/jobs/{id}/ack`
  `{printed, error?}` (failure requeues until the attempt cap).
- `GET /{tenant}/pos/settings` now returns `print_agent_online` so the till knows the server queue
  owns printing (and skips its own client auto-prints — double-print guard).

## Local Print Agent (cmd/print-agent, v1.2.0)

Loopback API (127.0.0.1:9330): `/health`, `/discover`, `/ping`, `/print` (now accepts `{name}` for
locally-installed USB printers, not just `{ip,port}`), `/printers` (enumerate OS printers),
`/pair` `{server, key}` (persist pairing → start spooler), `/status`.

Spooler (`spooler.go`): on pairing, long-polls the server, dispatches (`printer_ip` → `sendRaw`
9100; `printer_name` → OS spooler via `local_print_windows.go` [alexbrainman/printer RAW] or
`local_print_other.go` [`lp -d <name> -o raw`]), acks. Config persists in the OS config dir
(`codevertex-print-agent/agent.json`). Unpaired agents remain a dumb client relay (backward
compatible).

## pos-ui

- `lib/pos/print-jobs.ts` — server queue + pairing + loopback helpers.
- `lib/pos/printer-discovery.ts` `printProfileHtml` transport order: **agent (ESC/POS raw, network
  or local-name)** → QZ Tray (HTML) → browser window (disabled entirely under `silent`).
- Settings → Receipt & Printing: `PrintAgentCard` (pair this terminal, liveness pills, one-time key
  fallback); Test print now uses server-built ESC/POS + silent transports for ALL real printer
  types — a browser dialog only for profiles explicitly set to "Browser dialog".
- `OrderPlacedDialog`: logout is immediate. Agent online → the server queue owns the bill; else the
  client print is fire-and-forget (never awaited before `router.replace`).
- `terminal-context` / `terminal-modals`: client `printKitchenBarTickets` and ReceiptPreview
  auto-print are skipped when `print_agent_online` (server enqueued them already).
- `ReceiptPreview` Print button: queue → agent/QZ silently; "Save PDF" is always the browser window.

## Retail receipt template (2026-07-14)

Outlets with `use_case = "retail"` render a boxed invoice-style receipt (BOI/GoDigital design)
on every surface, selected automatically from `ReceiptView.UseCase`:

- **Server HTML/PDF** — `generateRetailReceiptHTML` / `generateRetailReceiptPDF`
  (`internal/http/handlers/receipt_retail.go`): boxed business header (logo honours the
  show-logo setting), `Customer | INVOICE.NO | DATE` table, `SERVED BY`, bordered items table
  (Item/Qty/Price/Subtotal), totals block (Total Quantity, TOTAL ITEMS, Subtotal, itemised
  Discount/VAT/named charges/Round Off, TOTAL, payment method with settle date e.g.
  `Cash (14-07-2026)`, AMOUNT PAID, `Total Due with Current`), a **Code 128 barcode** of the
  order number (`printing.Code128PNG`, boombuler/barcode), the configurable footer text
  (`receipt_footer` — the "IN GOD WE TRUST" position, flows below the barcode) and the provider
  advertisement in smaller print.
- **ESC/POS** — retail customer receipts additionally print a native `GS k` CODE128 barcode of
  the order number, the payment date beside the method, Amount Paid and a Balance Due line.
- **pos-ui** — `RetailReceiptPrint` (`components/pos/receipt-retail-print.tsx`) renders the same
  design client-side (preview print root, print window, Save-PDF via an A4 document shell).

New `ReceiptView`/JSON fields: `use_case`, `show_logo`, `payment_date`, `balance_due`
(total − collected; on-account credit sales carry the full amount due), `charges` (named
breakdown) and `barcode_png` (data URI).

## Receipt settings additions

- `show_logo_on_receipt` (GET/PUT `/pos/settings`) — include the tenant/outlet logo on
  generated receipts. Stored in `OutletSetting.metadata.receipt_show_logo` (no migration);
  defaults to true. Honoured by the classic + retail HTML/PDF renderers and the pos-ui
  preview/print components. Toggle lives in Settings → Receipt & Printing → Receipt Content.

## Credit-sale payment status (2026-07-14)

`paid_total` now counts only money actually collected: the on-account tender row is excluded
(`RecomputePaidTotal` returns `(collected, settled)`; completion/reopen key on *settled*). A
credit sale therefore reads **due/partial → overdue** (never "paid") on All-Sales/orders lists,
matches the paid/partial/due/overdue filters, and its receipt shows `Credit (on account)` with
Amount Paid 0 and the full Balance Due.
