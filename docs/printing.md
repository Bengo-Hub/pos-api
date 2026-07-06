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
