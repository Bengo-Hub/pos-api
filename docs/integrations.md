# POS Service — Integration Guide

**Last updated:** 2026-05-21

> **Retail-POS revamp note (2026-06-07):** several ❌ statuses below are **stale**. Verified against
> code 2026-06-07: pos **loyalty auto-earn on `pos.sale.finalized` IS wired** (`SaleFinalizedSubscriber`),
> inventory **publishes `inventory.purchase_order.received`**, and treasury **AR/AP aging endpoints
> are live** (`/ar/aging`, `/ap/aging`). **Loyalty is now the pos-api source of truth** (keyed on
> `crm_contact_id`; ordering-backend is a client). New retail surfaces (pricing profile, parked
> sales, return-by-invoice, credit sale→AR, cheque/bank/advance/store-credit tenders, M-Pesa C2B
> cashier reconciliation, add-expense-from-register) are scoped in the gap matrix. Authoritative
> current state + roadmap: `/.claude/plans/_audit-parts/retail-pos-gap-matrix.md` and
> `/.claude/plans/_audit-parts/retail-pos-audit-and-roadmap-2026-06-07.md`. Re-verify any specific ❌ line against
> code before relying on it.

## Overview

The POS service is the **source of truth for sales catalogs (menus)**. While `inventory-api` owns the physical item master, `pos-api` owns how those items are grouped, priced, and displayed for sale at an outlet.

---

## eTIMS Ownership — Architecture Decision Record

**Decision (2026-05-09):** KRA eTIMS fiscal submission is owned by **treasury-api**, not pos-api.

**Rationale:**
- treasury-api already owns all invoicing, tax calculation, and payment settlement for the Codevertex platform.
- eTIMS is a tax-invoice signing and transmission obligation — it belongs alongside the invoicing ledger, not the POS transaction recorder.
- pos-api is a **thin client** for non-cash payments: it creates a payment intent in treasury-api, then reacts to the result. eTIMS follows the same pattern — pos-api passes invoice data to treasury-api, which handles signing, KRA transmission, and QR code generation.
- This avoids duplicating KRA API credentials and the OSCU/VSCU device serial across two services.

**Correct flow for eTIMS:**
```
1. pos-api completes an order → publishes pos.sale.finalized (outbox)
2. treasury-api consumes pos.sale.finalized
3. treasury-api submits invoice to KRA eTIMS API (OSCU/VSCU mode)
4. treasury-api stores record in etims_invoices table, publishes treasury.etims.invoice_transmitted
5. pos-api consumes treasury.etims.invoice_transmitted → stores etims_invoice_number + qr_code_url on pos_order
6. pos-ui receipt renders eTIMS QR code from the pos_order response (not directly from treasury-api)
```

**VAT on a discounted sale (2026-11-13).** Each line of `pos.sale.finalized` and of the sync sign
request carries `paid_gross` and the VAT in it (`saletax.OrderDiscountVAT`, through the shared
`httpware/tax` calculator). A deal stamped on one line (happy hour or buy one get one,
`metadata.discount_amount`) comes off that line only, at its own rate. Only the rest of
`discount_total`, a discount on the whole bill, is shared across all lines by gross. The receipt's
VAT by rate and the line-level reports use the same split. Before this, a one-item deal was spread
over every line, so per-line VAT and the eTIMS band totals were wrong on mixed-band sales although
the total VAT was right. A manual line discount from the till lowers the line price itself, so it
was never part of `discount_total`.

**What pos-api does NOT do:**
- pos-api does NOT call the KRA eTIMS API directly.
- pos-api does NOT store the raw KRA API credentials (ETIMS_URL, ETIMS_CU_SERIAL, ETIMS_API_KEY).
- pos-api does NOT own an EtimsInvoice entity — treasury-api owns that (table: `etims_invoices`).
- pos-api does NOT own quotations or sales credit-notes either — treasury owns ALL financial documents
  (invoices, quotations, proforma, sales credit-notes). A POS sale has no invoice row, so a return or
  reversal of a sale that was transmitted to KRA sends the returned lines, the outlet and the original
  order id to treasury, which raises the credit note and sends it to KRA as a refund. With a refund to
  settle they ride on `POST /api/v1/s2s/{tenant}/refunds`; otherwise pos-api calls
  `POST /api/v1/s2s/{tenant}/pos/returns/credit-note` (`treasury.Client.RaiseReturnCreditNote`). Lines are
  built by `orders.BuildRefundLines`, priced the way the sale was sent. A pos "Save as Quotation" calls a
  treasury quotation S2S create.
  The refund method (cash, M-Pesa, MTN MoMo, Airtel Money, Other, card, bank, cheque, store credit,
  reduce customer credit) only changes the books, never what KRA receives. "Reduce customer credit"
  (`offset_invoice`) is allowed only on a sale settled on account; a sale paid in full has no balance
  to reduce (`returns/policy.go`).
  Refunds go back the way the sale was paid (2026-11-12). A refund return with no method chosen, a sale
  void or edit with no method chosen, and a voided payment all use the sale's original method
  (`payments.OriginalRefundChannel`: the money tender that carried the most; credit, store credit,
  exchange credit, loyalty and complimentary tenders are skipped; cash when nothing else applies).
  The channel sent to treasury is `mpesa_manual` (an M-Pesa code payment), `mpesa` (STK or C2B),
  `mtn_momo_manual`, `airtel_money_manual`, `other_manual`, `card_manual` (PDQ or card), `bank`,
  `cheque` or `cash`. Treasury first books the refund to the tenant's account that defaults to that
  method (so a code sale paid into the Till account is refunded from it), else `refundChannelToCode`
  books M-Pesa, MTN and Airtel refunds against the mobile money float (1100), bank and cheque against
  the bank (1300), and cash, Other and card against cash (1000).
  The cashier can still pick another method, also for an exchange's leftover (a cashier who hands
  the leftover back in notes picks cash), and store credit and on-account offsets keep their own
  defaults. pos-ui sends a method only when the cashier picked one or the sale's original method is
  known, so the server default applies otherwise. Only a cash refund counts as cash out of the drawer.
  A split sale refunds every return through its largest money tender, whatever earlier returns
  already took from it, so one tender can be refunded more than it received (a sale paid 600 by
  M-Pesa and 400 cash refunds a 700 return entirely by M-Pesa); the cashier picks another method
  when that is wrong.
  Charge to Room (S2-20, 2026-11-12): `POST .../payments/intent` with `tenderMethod` `room_charge` and `roomId` writes the folio line (`pos_order_id` set, metadata `source: pos_room_charge`) and a completed payment in one transaction under the order and stay row locks, and completes the order with no treasury intent. It is idempotent per request: pos-ui sends `idempotencyKey` (one per Charge to Room screen plus the room, at most 100 characters; a longer key gets a 400). Without a key the server treats a request for the same order, room and amount as a retry, so an API client that splits one bill evenly onto one room must send keys. Either way a bill split across two rooms posts both and a hand-posted line never blocks it. A sale cannot carry both the complimentary payment method and a room charge (422 either way); the message tells the cashier to comp the free items as a discount (the manual discount, with its reason and manager approval) and charge the rest to the room, and a whole-bill comp still uses Complimentary. Treasury holds the room charge on the 1430 Guest Ledger until checkout, apart from the city ledger (1400 AR). It needs the till payment permission like any tender, and `paid_total` counts it as paid, since the debt sits on the folio. `pos.sale.finalized` carries `selling_scheme` `room_charge` (or `mixed`) and `room_charge_amount`, and a folio payment at checkout sends `pos_charge_amount`, the share clearing those lines, applied to the POS lines last (room nights first), so treasury credits the guest receivable instead of booking that revenue again. A folio credit for a return or reversal is capped at what the folio owes and at the sale's room-charged part (the till's folio lines for the order, which a reversal never nets) less earlier credits for the sale; a full void of a room-charged sale credits the folio for the room part too; the rest goes back as the `pos_return_rest` refund. `pos.return.completed` now carries `tax_amount` for every return.
  A sale charged to a room (`room_charge`) is credited back to the guest's room folio while the stay
  is open (owner decision 2026-11-12), whatever method was picked: the return or reversal stores the
  `room_folio` channel and writes a completed folio payment of method `return_credit`, referenced by
  the return or reversal number (idempotent, tenant scoped, backed by the partial unique index
  `roomfoliopayment_return_credit_ref`), which lowers what the guest pays at checkout. The credit is
  capped at what the folio still owes (its charges less its completed payments), so a bill the guest
  already settled is never driven below zero. Whatever the folio no longer owes is refunded through
  the method the cashier picked (cash by default) as a second treasury refund. That rest refund uses
  reference type `pos_return_rest` (treasury raises a credit-note document only for `pos_return`,
  and the main refund's credit note already carries every returned line), the deterministic id
  `uuid.NewSHA1(return_id, "folio-rest")` and the return's outlet. It is sent only after the main
  refund went through; otherwise the return keeps it as `folio_rest_pending` and the credit-note
  retry job (`RetryPendingFolioRests`, every 5 minutes, oldest first) sends it once. After 12 failed
  attempts (`folio_rest_attempts`) it stops, marks `folio_rest_failed` and logs the return id so staff
  can settle the rest by hand. `pos.return.completed` then
  carries `ledger_refund_amount` (the refund less the rest), the part booked under the return id,
  so treasury's GL backstop must book that rather than `refund_amount` (treasury change pending).
  Only the rest counts as cash out of the drawer, and the return records `folio_credit`,
  `folio_rest_amount`, `folio_rest_channel`, `folio_rest_tax` and `folio_rest_cost` in its metadata. A folio that owes nothing gets no credit
  and the whole refund goes through the picked method. Treasury gets the credited part as `room_folio`,
  which reduces the guest receivable the room charge was booked to (S2-20); a reversal sends any rest
  as its own cash refund under `pos_return_rest`, and `pos.return.completed` carries `refund_channel`
  `room_folio` and the lines' `tax_amount` so the GL backstop books the same. Since 2026-11-13 every `pos.return.completed` carries the `refund_channel` the return settled through (cash, mpesa_manual and the other manual tenders, store_credit, offset_invoice, room_folio), and a main refund that fails at completion is kept on the return (`treasury_refund_pending` with its amount, tax, cost, channel and currency) and resent by `RetryPendingRefunds` on the same 5 minute tick, oldest first, 12 attempts, then `treasury_refund_failed`; a folio rest waits for it, and a folio lookup error now fails the completion instead of refunding as cash. Every treasury refund (returns, reversals, layaway cancels, payment voids) now carries the sale's `outlet_id`, so treasury credits that outlet's own account for the channel. The stay is found through the folio item carrying the sale's
  order id. Once the stay is checked out the refund goes through the chosen method, cash by default;
  a reversal whose stay closed before its treasury step records the cash it actually paid instead of
  `room_folio`, while a retried step that finds the credit its first attempt made keeps `room_folio`
  even if the stay has closed since. The return detail carries `room_stay_open` so pos-ui can say the refund goes to the
  room bill.
  At a pay upfront check-in, a code another payment took between the first check and the write
  records nothing for the payment; the response carries `payment_recorded: false` and
  `payment_message` "Checked in, but the code was already used; take payment again."
  An exchange is a refund plus a new sale, as KRA treats it (2026-11-01): the return settles its full
  value with `exchange_credit` (the returned value, capped at the replacement total) so treasury keeps
  that part in exchange clearing (2310) and pays out only the leftover; the replacement order is sold at
  full price, never discounted, and paid with an `exchange_credit` tender (`payments.TenderExchangeCredit`)
  that treasury books against clearing. A replacement the credit covers completes through
  `payments.RecheckCompletion`, so `pos.sale.finalized` takes it to treasury, eTIMS and stock. Before
  this the replacement carried the credit as a discount, so KRA saw the returned value taken off twice.
  A standalone pos `CreditNote` entity was built once and DISCARDED — do not reintroduce it. pos-ui LINKS
  to treasury-ui for these documents (never duplicates the pages); only the create-from-pos action lives here.

**What pos-api does store:**
- `pos_orders.etims_invoice_number` (nullable string) — populated after `treasury.etims.invoice_transmitted` is received.
- `pos_orders.etims_qr_code_url` (nullable string) — populated from `treasury.etims.invoice_transmitted` payload.

**Environment variables (treasury-api, NOT pos-api):**
- `ETIMS_URL`, `ETIMS_CU_SERIAL`, `ETIMS_API_KEY` belong in treasury-api only.

**Sprint 12 (pos-api) correction:** Sprint 12 originally listed a `FiscalReceipt` schema and `POST /{tenant}/pos/fiscal/sign` endpoint inside pos-api. This is incorrect. Sprint 12 work in pos-api is limited to:
- Adding `etims_invoice_number` and `etims_qr_code_url` nullable fields to `pos_orders`.
- Adding the NATS subscriber for `treasury.etims.invoice_transmitted` to populate those fields.
- Ensuring receipt PDFs and pos-ui display the QR code from `pos_order` data.

See also: [Sprint 12](sprints/sprint-12-integrations-webhooks.md) and [Sprint 5](sprints/sprint-5-erp-gaps.md).

---

## 1. Inventory Service Integration

### 1.1 Catalog Sync (Background Worker)

**Trigger:** NATS `inventory.catalog.updated` event  
**Subscriber:** `internal/platform/events/subscribers.go`  
**Action:** Fetch updated items from `GET /v1/{tenant}/inventory/items`, upsert `catalog_items` projection

**Status:** ❌ NATS subscriber not yet wired (Sprint 6 — remaining gap)

### 1.2 Stock Consumption Backflush

**Trigger:** `pos.sale.finalized` outbox event (published on order completion)  
**Action:** pos-api calls `POST /v1/{tenant}/inventory/consumption`

```
POST https://inventoryapi.codevertexafrica.com/v1/{tenant}/inventory/consumption
X-API-Key: {INTERNAL_SERVICE_KEY}
Content-Type: application/json

{
  "pos_order_id": "uuid",
  "outlet_id": "uuid",
  "items": [
    { "sku": "ITEM-001", "quantity": 2.0, "uom_code": "EA" }
  ]
}
```

**Client:** `internal/modules/inventory/client.go` (S2S via `shared-service-client`)  
**Env vars:** `INVENTORY_SERVICE_URL`, `INTERNAL_SERVICE_KEY`  
**Retry:** 3 attempts with exponential backoff  
**Status:** ❌ HTTP call not yet wired in `orders.Service.Complete()` (Sprint 6 — remaining gap)

### 1.2b Transaction Reversal (platform-owner data-repair tool, 2026-07-17)

**Trigger:** Platform owner runs a reversal from pos-ui Sync Monitor → "Txn Reversals" tab
(`POST /api/v1/{tenant}/pos/reversals`, platform-owner JWT only).
**What it does:** Reverses a FINALIZED sale (whole order or selected items) across every
integrated service, each step tracked on the `pos_reversals` row (steps JSON) and retryable
(`POST /reversals/{id}/retry`):

1. `pos_totals` — soft-voids the lines; partial: `RecomputeTotals` + payments/paid_total
   netted down; full: order → `refunded`, totals/payments kept + stamped.
2. `inventory_consumption` — `POST /v1/{tenant}/inventory/consumption/reverse` (S2S):
   BOM-accurate add-back net of recorded shortfall, negative compensating consumption
   lines/dailies, idempotent on `pos-reversal-{id}`.
3. `treasury_gl` — reuses the returns settlement `POST /api/v1/s2s/{tenant}/refunds`
   (revenue+VAT reversal, catalog-source COGS reversal, AR netting, auto credit-note doc);
   idempotent on the reversal id. When a reversal is larger than the cash it netted back, it splits
   into a cash portion (`pos_return`, id `uuid.NewSHA1(reversal_id, "cash")`) and an AR write-off
   (`pos_reversal_rest`, id `uuid.NewSHA1(reversal_id, "ar")`). Treasury raises a credit-note
   document only for `pos_return` refunds, so the split raises one document, not two; the write-off
   books the same refund GL. The write-off is sent only after the cash portion went through; a
   failure fails the step, and the retry books each portion once under its deterministic id.
   Reversals publish no GL backstop event, so nothing else books them.
   The reversed amount splits by the tenders behind it (the payments netted for this reversal, or
   for a full-scope reversal, which keeps its payment rows, what the order's completed payments
   paid): real money goes back through the refund channel (cash, M-Pesa, card, room folio), customer
   and exchange credit go back as `store_credit` (own id `"store-credit"` when split), a
   complimentary part goes as channel `complimentary` (own id `"complimentary"`; treasury credits the
   complimentary expense, no money moves; no tax when the sale was wholly complimentary), a loyalty
   part goes with the money (treasury booked it as cash at the till), and the rest (on account, or
   edit-added value never collected) goes as `offset_invoice` (fixed 2026-11-13; before, every full
   void was written off against AR and non-money tenders were refunded as cash). The
   `complimentary` channel needs the treasury-api release that adds it, so treasury ships first.
4. `etims_credit_note` — when the sale was transmitted to KRA (`orders.IsFiscalized`), calls
   `RaiseReturnCreditNote` with the reversed lines, the sale's outlet and the order id; treasury keys the
   credit note on the reversal id, so a retry returns the same document. SKIPPED when the sale was never
   transmitted.

**Modules:** `internal/modules/reversals` (orchestrator), `handlers/reversals.go`,
`ent/schema/posreversal.go`. Distinct from POSReturn (customer returns lifecycle).

### 1.3 Stock Alert Subscriber

**Trigger:** NATS `inventory.stock.low` event  
**Action:** Create notification entry in `stock_alert_subscriptions` or call notifications-service  
**Status:** ❌ NATS subscriber not yet wired (Sprint 6)

### 1.4 Variants: sizes and colours (2026-10-29)

Each variant is its own inventory item (child) with its own SKU, barcode, price and stock, linked
to a display parent through `metadata.variant_of` (inventory-api `06e7cdf`). Stock, cost, tax, UOM
and eTIMS work per child SKU with no POS change.
- Catalog sweep reads `include=variants`: children are hidden and each parent carries `variants[]`
  (`{id (child item id), sku, name, price, attributes, barcode, is_active, image_url, on_hand,
  available}`). Each child's `POSCatalogOverride` applies to its variant (POS price, staff
  "unavailable" toggle); prices round up. A parent with no price shows its cheapest active variant
  and is not hidden by the zero-price gate. A search on a child's SKU, barcode or name finds the
  parent. Stock never affects availability (manual availability rule).
- Barcode lookup searches with `include=variants`. An exact barcode, then an exact SKU, wins; a
  scanned child comes back with `variant` (the same snapshot the server stamps on the line).
- Order lines: `CreateOrder`, `AddOrderLines` and sale-edit increases drop any client-sent
  `metadata.variant` and stamp `{id, sku, name, attributes, parent_id, parent_sku, parent_name}`
  when the line SKU is a child, filling `catalog_item_id` with the child id when missing. One
  inventory call per order (`skus=...&status=all`), cached per SKU in Redis for 10 minutes
  (`pos:variant:{tenant}:{sku}`, misses cached too). Best effort with a 3 s timeout: a slow
  inventory never fails a sale; the line just goes unstamped.
- Promotions still match the line SKU: a promotion scoped to the parent SKU does not cover its
  variants. Scope it to the variant SKUs or the category.

---

## 2. Treasury Service Integration

### 2.1 Payment Intent Workflow (Card / M-Pesa)

For non-cash tenders, pos-api creates a payment intent in treasury-api before recording the payment.

**Full flow:**
```
1. pos-ui → POST /{tenant}/pos/orders/{id}/payments
            { tender_type: "card"|"mpesa", amount: 1500, currency: "KES" }

2. pos-api payments handler:
   if cash → record immediately, auto-complete order
   if card/mpesa:
     → POST https://booksapi.codevertexafrica.com/api/v1/s2s/{tenant}/payments/intents
       X-API-Key: {INTERNAL_SERVICE_KEY}
       {
         "source_service": "pos",
         "reference_id": "<order_id>",
         "reference_type": "pos_order",
         "amount": 1500,
         "currency": "KES",
         "payment_method": "paystack"|"mpesa",
         "customer_id": "<customer_uuid>"
       }
     ← 201 Created (M-Pesa):  { "intent_id": "...", "checkout_request_id": "..." }
     ← 201 Created (Paystack): { "intent_id": "...", "authorization_url": "..." }
     → store intent_id in pos_payments.external_reference
     → return { status: "pending", intent_id, checkout_url|mpesa_request_id } to pos-ui

3. pos-ui:
   M-Pesa: show STK push waiting screen, poll GET /{tenant}/pos/orders/{id}/payments every 3s
   Card: redirect to authorization_url (Paystack checkout)

4. treasury.payment.success NATS event
   → pos-api marks pos_payments.status = "completed"
   → order auto-completed if fully paid

5. treasury.payment.failed NATS event
   → pos-api marks pos_payments.status = "failed"
   → notify pos-ui
```

**Client:** `internal/modules/treasury/client.go`  
**Env vars:** `TREASURY_SERVICE_URL=https://booksapi.codevertexafrica.com`, `INTERNAL_SERVICE_KEY`  
**Auth:** `X-API-Key: {INTERNAL_SERVICE_KEY}` header (S2S) — same shared key for all services  
**Status:** ❌ S2S intent creation not yet wired in `payments.Service.Record()` (Sprint 6)

> **Split tenders (2026-10-04):** treasury keeps one intent per `(tenant_id, reference_id)` and answers a repeated reference with the existing intent. The first payment on an order uses `payref.Build("POS", ...)` (for example `POS-D78B1A-06C29A3B1220`). Every later payment on the same order (another pending or completed payment occurred before it) uses `payref.Portion`, which appends `-P` and the first 8 hex digits of the payment id (`POS-D78B1A-06C29A3B1220-P79C886D2`). The live path and `TreasuryIntentReconciler` share `Service.intentReference`, so a retry still dedups. Before this, every portion after the first collapsed into the first intent: treasury recorded only part of a split sale's money, and a digital portion was handed an intent that had already succeeded. The GL was unaffected because `pos.sale.finalized` posts the full sale total. Treasury finds POS intents by `metadata.entity_id`, never by parsing the reference.

> **Schema note (2026-05-09):** The actual `pos_payments` Ent schema uses field `status` (not `payment_status`) and `external_reference` (not `provider_reference`). Store the treasury `intent_id` in `pos_payments.external_reference`. Valid `pos_payments.status` values: `pending`, `completed`, `failed`, `refunded`.

> **Publisher note (2026-05-09):** The current `internal/platform/events/publisher.go` only defines three methods: `PublishOrderCreated`, `PublishOrderStatusChanged`, `PublishPaymentRecorded`. The `pos.sale.finalized` and `pos.drawer.closed` events described in the Event Catalog are **planned but not yet published** — they must be added to `publisher.go` in Sprint 6.

### 2.1a Uganda/Kenya/Tanzania Payment Methods

Superseded. The direct MTN, Airtel and manual bank-transfer gateways (2026-08-07) were removed
from treasury; MTN, Airtel, other mobile money networks, card and bank deposits all run on
PayHero (2026-10-02) and, since 2026-10-05, are offered inside the PayHero tender below rather
than as tenders of their own.

### Which gateway tenders the till shows (2026-10-05)

`GET /{tenant}/pos/gateways` (`internal/modules/treasury/client.go GetPublicGateways`) maps
treasury's public pay-page list (`gateways` plus `payhero_methods`) to:

```json
{"mpesa":false,"paystack":false,"wallet":false,"cod":true,"complimentary":true,
 "payhero":true,"payhero_methods":["mpesa","airtel_money"],"mpesa_c2b":false}
```

Every gateway fails **closed**: when treasury cannot answer, the handler serves the last answer
that pod saw for the tenant and currency, else nothing. pos-ui's `usePOSGateways` also starts with
everything off. Cash, Card (PDQ), Credit Sale and Multiple Pay need no gateway and always show.

- `mpesa` means Daraja only (the tenant's own paybill or till): it drives the STK Push tender.
  `mpesa_c2b` equals it, since C2B till matching reads Daraja confirmations.
- `payhero` is its own gateway, like Paystack: one PayHero tender (PayHero's logo, a sublabel of
  its rails) whose tender method is `payhero`. It opens treasury's pay page on the PayHero modal,
  where the customer picks M-Pesa, Airtel, MTN, card or another rail for the outlet's currency;
  every rail is initiated with `gateway: "payhero"`, so it never falls back to Daraja.
- Before 2026-10-05 PayHero's rails were flattened into `mpesa` (with `mpesa_provider=payhero`),
  `mobile_money`, `payhero_card`, `payhero_bank` and `payhero_offline`, so a PayHero-only tenant
  showed STK Push, Paybill, Mobile Money, Card and Bank Deposit tenders that looked like Daraja,
  and most of them never opened the payment page.
- Treasury offers nothing from PayHero until the tenant's Team (vendor id) exists, because every
  PayHero charge needs it. PayHero's offline paybill appears only after the platform marks it
  verified (treasury `payhero.offline_paybill_verified`).
- The Room tender and its rooms fetch need the plan's `hotel_module` as well as a hospitality
  outlet; otherwise the rooms call is a 403 that pops "Subscription limit reached".

`OutletSetting.mtn_momo_number`/`airtel_money_number` (display-only merchant numbers, printed on
receipts when `show_payment_info_on_receipt` is on) are separate from the treasury gateway
credentials above — one is "what number to show the customer," the other is "how to actually
collect/confirm the payment."

**Codes at the hotel desk (2026-11-12).** The hotel desk (folio settlement and pay upfront check-in)
takes M-Pesa, MTN MoMo and Airtel Money codes and Other with the till's rules
(`payments.NormalizeManualCode`): an M-Pesa code must be present, MTN and Airtel IDs must be 6 to 30
letters or digits (dots and dashes allowed), and Other needs a 2 to 40 character `label` saying how the
guest paid, kept in the folio payment's metadata as `payment_label` and sent to treasury in the intent
metadata. Codes are uppercased. A code that already settled a completed till sale or a completed folio
payment of the same network in the tenant is refused with 409 (`payments.ManualCodeUsed`); a bad format
is 400. At pay upfront check-in the code is checked before the guest is checked in. These tenders
settle immediately and treasury records each as itself, as at the till. pos-ui shows MTN and Airtel
at the desk only where the outlet has that number set. The till's own duplicate check still looks at
till payments only, so a code used first at the desk is not yet refused at the till.

### 2.1b Multi-Currency & Forex (2026-08-07)

Treasury is the single source of truth for currency conversion — pos-api never computes an
exchange rate itself. See `internal/modules/treasury/client.go`:
- `ListSupportedCurrencies` → proxies treasury's `GET /currencies` (ISO 4217 list with
  per-currency decimal places — feeds the pos-ui outlet-currency picker).
- `ConvertCurrency` → proxies treasury's `GET /exchange-rates/convert` (KES-pivoted, so any two
  supported currencies convert even though treasury only fetches KES-based rates live).

Exposed to pos-ui via `GET /{tenant}/pos/currency/currencies` and
`GET /{tenant}/pos/currency/convert` (`internal/http/handlers/payments.go`). The pos-ui Settings →
General currency picker calls the convert endpoint to show a live-rate confirmation
(`CurrencyChangeConfirmModal` from `@bengo-hub/shared-ui-lib`) before an outlet's currency is
actually changed — never applies silently.

`PaymentIntent`/`PaymentTransaction`/`ARTransaction` in treasury auto-populate a best-effort
`exchange_rate`/`base_amount` snapshot (converted to KES) whenever a transaction's currency isn't
already KES — see treasury-api's `EntRepository.convertToBase`. pos-api does not need to compute
or store this itself; it's populated treasury-side transparently.

### 2.2 Room Charge Settlement (Hotel Module)

On hotel check-out, pos-api creates a single treasury payment intent for the full folio amount.

```
POST /api/v1/s2s/{tenant}/payments/intents
X-API-Key: {INTERNAL_SERVICE_KEY}

{
  "source_service": "pos",
  "reference_id": "<room_guest_id>",
  "reference_type": "room_folio",
  "amount": <total_folio_amount>,
  "currency": "KES",
  "payment_method": "cash"|"paystack"|"mpesa"
}
```

**Status:** ❌ Not yet implemented (requires hotel module — Sprint 3 + Sprint 6)

### 2.2a Credit settlement sync to treasury AR (updated 2026-11-13)

When a customer pays off a sale taken on account, `SettleCreditPayment` records the payment in POS and posts it to treasury with `POST /api/v1/s2s/{tenant}/ar/customers/{key}/payment`. The key is the customer's CRM contact id, else the phone, else `staff:<id>` for a staff credit sale; the phone also goes in `customer_identifier` as a fallback. On success I set `payment_data.treasury_synced_at` (plus `treasury_receipt_id` and `treasury_customer_key` when treasury returns a receipt), which takes the row out of the retry set for good.

If that post fails, `CreditSettlementSyncReconciler` (`internal/modules/payments/credit_settlement_reconciler.go`) retries it every 2 minutes (one replica per period) with the same call. A row written in the last 2 minutes is left alone so the reconciler never races the live request. How a failure is handled depends on what treasury said:

| Treasury answer | What the reconciler does |
|---|---|
| Network error, timeout or 5xx | Retries on the next tick. |
| 400 `payment N exceeds outstanding debit X` | Terminal. Treasury already holds the money for that customer, so the same post is refused forever. I stamp `payment_data.treasury_sync_blocked` with `{reason: "exceeds_outstanding_debit", at, http_status: 400, detail}`, log one warning with the order number and payment id, and never post the row again. |
| Any other 4xx | Sets `payment_data.last_sync_attempt_at` and waits 15 minutes before the next try. |
| (no call) the order has no customer phone and no staff party | Terminal. Stamped `treasury_sync_blocked` with `reason: "no_customer_key"` and no `http_status`, logged once. |

The candidate query skips any row carrying `treasury_synced_at`, `treasury_receipt_id` or `treasury_sync_blocked`. A blocked row is for the AR drift audit or a person to settle: check the customer's receipts in treasury (`GET /api/v1/{tenant}/ar/customers/{key}/receipts`), and if the matching receipt is there, set `treasury_synced_at` on the row by hand; if it is genuinely missing, record it in treasury and then set `treasury_synced_at`. Deleting `treasury_sync_blocked` puts the row back in the retry set.

On 2026-11-13 I checked the eight boi-enterprises settlements that had been refused this way since August (orders 000278, 000304, 000187, 001252, 000228, 001976). Treasury held a matching receipt for every one, usually under a manual or backfill reference, which is why the post kept being refused.

### 2.3 Cash Drawer Close → Treasury Ledger

**Trigger:** Cash drawer close  
**Action:** pos-api publishes `pos.drawer.closed` outbox event  
**treasury-api** subscribes and creates a ledger entry for the cash position

**Status:** ✅ Event published (outbox). ❌ Treasury NATS subscriber not yet wired (treasury-api responsibility — Sprint 3)

### 2.4 NATS Events from Treasury

| Event | Action in pos-api |
|-------|-------------------|
| `treasury.payment.success` | Mark `pos_payments.status = completed`, auto-complete order |
| `treasury.payment.failed` | Mark `pos_payments.status = failed`, notify pos-ui |
| `treasury.etims.invoice_transmitted` | Populate `pos_orders.etims_invoice_number` + `etims_qr_code_url` for receipt printing |

**Status:** ❌ NATS subscribers not yet wired (Sprint 6 for payment events; Sprint 12 for eTIMS event)

---

## 3. Ordering Backend Integration

### 3.1 Catalog Sync (outbound)

pos-api publishes `pos.menu.updated` on any `CatalogItem` or `CatalogCategory` change.  
ordering-backend subscribes and updates its storefront projection.

**Status:** ✅ Event published on catalog write operations.

### 3.2 Online orders into POS and the KDS

The full cross-service flow is in shared-docs `docs/architecture/online-order-fulfilment.md`. On the pos-api side:

- **Ingestion.** `ConfirmedOrderConsumer` (`internal/modules/orders/confirmed_consumer.go`) is the only path. `ordering.order.awaiting_acceptance` creates the POS record on hold (no tickets); `ordering.order.confirmed` creates it live or releases the held one. Pickup becomes a `takeaway` order numbered `CC-`, delivery a `delivery` order numbered `DL-`; both go through `Service.CreateOrder`, so station routing, modifiers, tickets and chits match a till order. Service bookings become appointments. `ordering.order.for_pickup` is not consumed: ordering sends it when a pickup is ready, for the customer message.
- **Tickets.** `Service.issueKDSTickets` is the single creation path (new order, items added to a bill, fired course). It respects the outlet's `enable_kds` switch (printer-only kitchens get chits, no tickets) and pushes `ticket_created` with `order_subtype`, `order_source` and `channel`.
- **Channel.** `internal/modules/orderchannel` classifies every order once: `dine_in`, `takeaway`, `delivery`, `room_service`, `bar_tab`, `retail`, `service_job`, `online_pickup`, `online_delivery`. An order is online when `metadata.online_order_id` is set. The KDS list (`GET /kds/tickets`) returns `channel`, `order_source`, a route label (table, room or online channel with promised time) and the customer name for counter handovers. pos-ui groups and counts the board on `channel` only.
- **Progress back to ordering.** First KDS Start publishes `pos.online_order.preparing`; all tickets ready, or the queue's Ready, publishes `pos.kds.order.ready`; handover publishes `pos.online_order.collected` (pickup) or `pos.online_order.delivered` (own-staff delivery).
- **Lifecycle from ordering.** Cancellations, out for delivery and delivered/completed are mirrored onto the POS copy (`ordering_lifecycle.go`) and close its open tickets.

**Workflow per use case.** `outletpolicy.WorkflowFor(useCase, subtype)` decides where an order goes, for till and online orders alike:

| Outlet use case | Order | Goes to |
|---|---|---|
| hospitality, quick service | dine-in, takeaway, delivery, room service, bar tab | opens at once; KDS tickets on the routed stations and kitchen/bar chits; takeaway and delivery then wait in the pickup or delivery queue |
| retail, services (goods) | takeaway (collect), delivery | opens at once straight into the pickup or delivery queue; no kitchen ticket or chit; the customer bill prints as the pick list when the outlet auto-prints orders |
| any | counter sale (`retail`) | stays a draft until paid |
| services | `service_job` | opens onto the production board |
| services | online booking | an appointment, never a POS order |

Ticket issuing, held-order release, add-to-draft, delta chits and the ordering status subscriber all go through it, so a shop order cannot reach a kitchen by any path.

**Chit labels.** Every kitchen/bar chit (print agent and till alike) prints the order type large (DINE-IN, TAKEAWAY, DELIVERY, ROOM SERVICE, BAR TAB, ONLINE PICKUP, ONLINE DELIVERY) and the source (POS, or the online store with its order number), the customer to call for a counter handover, a promised time and the order note. Added-items and course chits keep the order type under their banner. Server: `printing.StationOrderLabel`; till: `chitLabelFor` in pos-ui `src/lib/kds/board.ts`.

### 3.3 Till deliveries and logistics

A delivery order rung up at the till is dispatched from the POS queue (`assign-rider`, `dispatch-delivery`) straight to logistics-api with `source_service = pos`, the POS order id as `external_reference`, the outlet as pickup point and the unpaid balance as `metadata.cash_on_delivery`. `LogisticsDeliverySubscriber` (`logistics_delivery.go`) consumes `logistics.task.*` and stores the rider's progress in `metadata.dispatch_status` (`dispatched`, `rider_assigned`, `rider_arriving`, `out_for_delivery`, `delivered`, `delivery_failed`, `dispatch_cancelled`) with the rider's name and phone. Progress never moves backwards; a failed or cancelled dispatch clears `logistics_task_id` so the order can be sent again. A delivered order closes only when the till already holds full payment; otherwise it waits on the queue with the cash the rider collected (`cod_*` metadata) until the cashier settles it.

The pickup and delivery queues share one live-queue rule: not cancelled, voided, refunded or a parked draft, not yet collected, and a till order already paid at the till drops off after 24 hours. A till order cannot be handed over before it is paid.

---

### 3.4 Till delivery pricing (2026-10-10)

The till prices deliveries with logistics' delivery areas, the same quote online checkout uses; pos-api keeps no fee tables. The terminal's delivery panel lists the tenant's areas (`GET /pos/delivery-areas`, from logistics `GET /s2s/zones/{tenant}/coverage`), and the chosen area's pin is previewed with `GET /pos/delivery-quote`. On `POST /pos/orders` with `order_subtype = delivery` and `metadata.delivery_lat/lng`, `applyDeliveryQuote` re-quotes through `POST /s2s/zones/{tenant}/quote`, sets `charges.shipping` to the fee (a typed charge is replaced) and stores `metadata.delivery_quote` (fee, zone, distance, time). The quoted fee is not a manager adjustment, so it skips the `order.adjustment` approval. A pin outside the areas returns 422 `delivery_not_serviceable`. If logistics is unreachable the cashier's typed charge stays and needs approval as before.

### 3.5 Storefront promotions (S2S, `X-API-Key`)

pos-api owns every discount; the storefront only reads them.
- `GET /api/v1/s2s/{tenant}/discounts?status=active` feeds a tenant's Top Deals grid; ordering keeps only promotions inside their window.
- `GET /api/v1/s2s/{tenant}/discounts/banners?use_case=` feeds the banner carousel: active promotions with `metadata.banner.show_on_storefront`, rechecked against the tenant's `storefront_banner` feature on every read. Since 2026-10-27 each banner also carries `is_flash_sale` and `end_at`. Both fields were declared before but never filled, so a flash-sale banner could not count down.
- `GET /api/v1/s2s/storefront/discounts?tenant_ids=a,b,c` (2026-10-27, at most 50 tenants) returns, for the platform marketplace page, every promotion of those tenants that is live now and flagged for the storefront (a banner or a flash sale), with its rule and `tenant_id`, soonest ending first. It is one query for the promotions and one for their rules, whatever the number of stores. Banner-only promotions keep the live feature recheck.
- `GET /api/v1/s2s/{tenant}/pos/sales/by-sku?from=&to=&order=quantity&limit=` returns real units sold per SKU; the storefront's best sellers pass `order` and `limit` so only the top rows come back.

## 3A. MarketFlow CRM Integration (November 2026)

marketflow-api owns customer identity. pos-api keeps `crm_contact_id` on loyalty accounts and client records, plus the name, phone and email as a read-only snapshot.

- **Client.** `github.com/Bengo-Hub/shared-service-client/crm` (v0.5.0), configured with `MARKETFLOW_API_URL` and `INTERNAL_SERVICE_KEY`.
- **Creating a customer.** Staff-created loyalty accounts and client records upsert the contact first. A CRM answer of 400 or 422 is shown to the user. An outage (401, 403, 429, 5xx, no answer) saves the record unlinked within 2 seconds, and the reconciler links it later. A sale never waits on the CRM.
- **Phone lookup.** One indexed lookup matches every common spelling of a number (+254..., 254..., 07..., bare digits). Among duplicate accounts the one with the most lifetime points wins, then the CRM-linked one, then the oldest, then the lowest id. A merged shell (see below) is never picked: the lookup moves on to the account it was merged into. A new account is never created when one exists.
- **Snapshots.** `crm.contact.updated` refreshes the snapshot from the CRM. `crm.contact.deleted` clears the link and sets `metadata.crm_unlinked_at` on the loyalty account or client record, and the reconciler skips marked rows. A staff create or edit clears the mark and links again.
- **Loyalty is never destroyed by a customer delete.** When treasury deletes a customer, POS deletes its rows except a loyalty account that still holds points. That one is unlinked and marked instead.
- **Hotel guests.** The guest register keeps the details typed at check-in and never takes data from the CRM. Check-in and staff booking creation link the guest by sending only the phone, email and name, never the ID number, nationality or party size. When staff edit a guest or a booking that has no CRM link yet, I link it the same way, with the phone, email and name as they stand after the edit and within the same 2 second budget. A cancelled booking is not a customer relationship, so cancelling a booking, or editing one already cancelled, never links it. Only the contact id is stored, so the register keeps what the front desk typed. A CRM refusal or outage never blocks a check-in, booking or edit: the record is saved unlinked. The reconciler does not cover guests or bookings, so an unlinked record stays unlinked until its next edit, check-in or booking. The public booking widget never links, because an unauthenticated visitor must not be able to create CRM contacts; the first staff edit of such a booking links it.
- **Contacts picked at the desk are checked.** Hotel notices go to the stored contact, so only a contact the server linked itself is trusted (2026-11-12). A `crm_contact_id` sent with a check-in, room booking or event booking is kept only when the CRM holds that person's phone or email on the contact (`customers.Resolver.ContactMatches`, one batch lookup within the 2 second budget). Otherwise it is dropped and the person is linked by their own phone, email and name as usual. An id the CRM cannot be asked about (outage) is not trusted either. A check-in from a booking keeps the booking's stored contact without a lookup, since the server linked it. The check-in, room booking and event booking responses carry `crm_link: {status, message}`: `linked`, `mismatch` (the picked contact is not this person, and the guest was linked to their own contact instead) or `unlinked` (no contact id; the message says why, including when the picked contact could not be checked because the CRM was down or the guest has no phone or email, so it was not used). pos-ui shows the message; for `unlinked` it offers Add to CRM, which is an empty guest edit (`PATCH .../guest` with `{}`) and links an unlinked guest. The desk's code duplicate check runs again right before the folio payment is written. The booking responses keep every booking field at the top level.

- **Erasure link check.** When a person is erased in the identity service, marketflow asks every service whether it still links the CRM contact before it deletes it. POS answers `GET /api/v1/s2s/{tenant}/crm-links/{contactID}` with `{"referenced": true|false}`, using the shared `X-API-Key`. `{tenant}` is the tenant UUID or slug. The answer is true when any row still carries the contact id, in any tenant (CRM contact ids are random UUIDs minted by marketflow, so a match anywhere is the same person, and a tenant mismatch can only answer "referenced", which keeps the contact), in loyalty accounts (merged shells included, to stay on the safe side), client records, customer balances, appointments, room bookings, room guests or event bookings. Only the flag is returned, and logs carry ids only. A bad contact id answers 400, a tenant POS does not hold (unknown slug, or a UUID with no local tenant row, as after a tenant id drift) 404, and a missing or wrong key 401. marketflow treats a 404 as "cannot answer" and keeps the contact.
- **Notification events carry ids, not personal data.** For a person linked to the CRM, `pos.loyalty.points.earned`, `pos.loyalty.tier_upgraded`, `pos.loyalty.referral_earned`, `pos.hotel.check_in` and `pos.hotel.check_out` carry `crm_contact_id` plus the record ids, and no name, email or phone. notifications-api looks the details up in the CRM when it sends. `crm_contact_id` is always the person the notice goes to: for `loyalty.referral_earned` that is the referrer, and the referred friend appears only as `referred_crm_contact_id` (or `referred_phone` when the friend has no linked account). marketflow's tier update uses the same id. A person with no CRM link (the CRM was down or refused the contact, or the record predates linking) still gets `customer_*`, `referrer_*` or `guest_*` name, email and phone as before, so their notices keep arriving; this fallback stays until every record is linked. For hotel guests only the event changes: the guest register keeps what the guest gave at the desk.
- **Layaway reminders and receipt requests.** `pos.layaway.payment_due` follows the same rule: for a customer linked to the CRM it carries `layaway_plan_id`, `balance_due`, `due_date` and `crm_contact_id`, with no name, phone or email. The contact is the one on the loyalty account the plan was opened on, else the account holding the plan's phone, in the plan's own tenant; a staff layaway is never linked. `pos.sale.notification_requested` (the "send receipt" action) carries `crm_contact_id` and no name for a linked customer, found through the loyalty account on the sale's phone in the sale's tenant. A phone or email the cashier typed for that one send still travels as `customer_phone` or `customer_email`, because that is where the receipt must go, and notifications-api prefers it over the CRM's. An unlinked customer keeps `customer_name`, `customer_phone` and `customer_email` on both events.
- **Refunds.** `pos.return.completed` (and `pos.exchange.completed`, which shares the payload) carries `crm_contact_id` and no name or phone when the buyer's phone belongs to a CRM-linked loyalty account in the sale's tenant; otherwise it keeps `customer_name` and `customer_phone` (plus an empty `customer_email`). inventory-api and treasury's GL backstop read only ids and amounts from it.
- **Payment confirmations.** `pos.payment.received_notification` (the View Payments "notify" action) follows the same rule: `crm_contact_id` and no name or phone when the sale's phone belongs to a CRM-linked loyalty account in the sale's tenant, else `customer_name` and `customer_phone` (plus an empty `customer_email`).
- **Not covered yet.** The automatic receipt on `pos.sale.finalized` still carries the customer's name and phone. `sale.finalized` also feeds treasury's customer ledger, which reads them.

### Merging duplicate loyalty accounts

Older tills saved the same customer more than once when the phone was typed in a different way (0712..., 254712..., +254712...). I merge those accounts with a one-off tool. It is safe to run more than once.

**What it does.** For each tenant it groups loyalty accounts whose phones are the same number once written in full international form. Kenyan spellings group together. A foreign number never groups with a Kenyan one, and a phone that is not a valid number is never grouped. In each group the survivor is the account a phone lookup would pick when no CRM contact is given: the one with the most lifetime points; on a tie, the one linked to the CRM; then the oldest; then the lowest id. The survivor gains the points and lifetime points of the others and takes over their loyalty transactions, referrals, layaway plans and loyalty tender payments. It keeps its own CRM link. If it has none, it takes the link of the other account with the most lifetime points that has one. The other accounts stay behind as empty, unlinked shells with `merged_into`, `merged_at` and `merged_reason` in their metadata; a shell that was linked also keeps its old contact id as `merged_crm_contact_id`, and the report flags the group with `contact_conflict` when the accounts were linked to different contacts, so I can check those by hand. Each group runs in its own transaction; if an account changes while it is being merged (a sale in the middle, say), that group is refused and shown in the report, and a second run picks it up.

**After the merge.** Phone lookups that land on a shell move on to its survivor, and the reconciler never links a shell again. Earning points on a shell's id (from a till that still shows the old account) credits the survivor. Redeeming points or paying with points on a shell's id answers 409 with the code `loyalty_account_merged` and the survivor's id, so the till looks the customer up again before taking points. The till does this by itself: it switches the attached customer to the survivor and reloads the balance, so the cashier only presses redeem again. The account list hides shells; add `include_merged=true` to see them. When a customer delete in treasury removes a survivor with no points, its empty shells are removed in the same transaction. If a shell is ever left pointing at a missing account, registering that number again turns the shell back into an ordinary empty account (its old marker is kept under `orphaned_merge`) instead of failing.

**Dry run first.** The tool only reports unless you ask it to merge. It needs a platform owner token.

```
POST /api/v1/recovery/loyalty-merge-duplicates
{"tenant_id": "<tenant uuid>"}
```

Send an empty body to cover every tenant. The answer lists each group with the tenant, the phone with all but its last three digits hidden, the account ids with their points, lifetime points and CRM link, the survivor, the reason it was chosen and the contact conflict flag. Names, emails and full phone numbers are never returned or logged. A body that is not exactly this JSON (a wrong type, an unknown field) is refused with 400 and nothing runs.

**Real run, one tenant at a time.** Send the same request with `"dry_run": false`:

```
POST /api/v1/recovery/loyalty-merge-duplicates
{"tenant_id": "<tenant uuid>", "dry_run": false}
```

I run the real merge per tenant, after reading that tenant's dry run, so each run stays short and its report is easy to check. A real run without a `tenant_id` is refused unless the body also says `"all_tenants": true`. Each group then shows `merged` or `refused` with the reason. Running it again reports no groups.

---

## 4. Auth Service Integration

### 4.1 JWT Validation (SSO Login)

All pos-api protected routes under `/{tenant}/pos/` validate Bearer tokens issued by auth-api (RS256, audience `codevertex`).

**Library:** `shared/auth-client` v0.1.0  
**Env vars:** `AUTH_SERVICE_URL`, `AUTH_AUDIENCE=codevertex`  
**Status:** ✅ Implemented

**Flow:**
1. pos-ui redirects to auth-api OAuth2 PKCE endpoint (`/oauth2/authorize`)
2. User logs in via SSO (Google, Microsoft) or email/password
3. auth-api issues access token (15 min) + refresh token (30 days)
4. pos-ui stores tokens in localStorage, sends `Authorization: Bearer {token}` on all API calls

**Suitable for:** Manager, admin, and office-based staff who have SSO accounts

### 4.2 Terminal PIN Login (CRITICAL GAP)

**Background:** The hotel-pos-v8.jsx design requires a touchscreen PIN login (4–6 digits) for kitchen staff, waiters, cashiers, bar staff, and receptionists. These users cannot go through a browser OAuth2 redirect on a dedicated POS terminal.

**Current state (2026-05-21):** ✅ IMPLEMENTED. PIN login is fully operational.

**Implemented:**
- `pin_hash`, `pin_failed_attempts`, `pin_locked_until` fields on `staff_members` Ent schema
- `POST /{tenant}/pos/auth/pin` — validates PIN, issues 4-hour terminal JWT (HMAC-SHA256)
- `POST /{tenant}/pos/auth/pin/set` — manager sets/resets staff PIN (requires `pos.staff.manage`)
- `GET /{tenant}/pos/staff` — list staff for PIN selector grid (public route)
- `GET /{tenant}/pos/auth/pin/profile` — staff profile for offline PIN auth
- Terminal JWT embeds: `outlet_id`, `outlet_code`, `outlet_use_case`, `is_hq_user`
- pos-ui: PIN kiosk at `/[orgSlug]/pin-login` with avatar grid + 12-button keypad
- Offline PIN validation via bcrypt against IndexedDB cached `staffProfiles`
- `GET /{tenant}/pos/auth/me` — Trinity Layer 3: returns POS role + permissions for SSO users

**Token type:** `pos_terminal` HMAC-SHA256 JWT, 4-hour expiry, stored in `sessionStorage`  
**Status:** ✅ Complete — see [pos-ui Sprint 10](../../pos-ui/docs/sprints/sprint-10-pos-auth.md)

### 4.3 Tenant renames (`auth.tenant.updated`, 2026-11-12)

Auth owns the tenant slug. When auth renames a tenant (same UUID, new slug), I move the local tenant row and every outlet of that tenant still carrying the old slug onto the new one in one transaction, then drop the cached tenant under both slugs. The outlets must move with the tenant because receipts pass the outlet slug to treasury for eTIMS and account balances. The tenant syncer does the same when it meets the new slug on its update path. A slug already held by another local tenant is refused and logged once, not retried. Terminals paired before a rename carry the old slug in their token and need to pair again.

---

## 5. Webhook Delivery System (Sprint 12)

### Webhook Subscription CRUD

Registered in router under `/{tenantID}/pos/webhooks/`:

| Method | Path | Description |
|--------|------|-------------|
| GET | `/webhooks` | List subscriptions |
| POST | `/webhooks` | Create subscription (URL, events, secret) |
| PUT | `/webhooks/{id}` | Update subscription |
| DELETE | `/webhooks/{id}` | Remove subscription |
| GET | `/webhooks/{id}/deliveries` | Delivery log |

**Schemas:** `WebhookSubscription` (`webhooksubscription.go`), `WebhookDelivery` (`webhookdelivery.go`)  
**Status:** ✅ CRUD implemented. ❌ Delivery worker (NATS fan-out, retry, HMAC signing) not yet implemented.

---

## 6. Online Order Pickup (Sprint 13)

### Click-and-Collect Pickup Endpoints

Registered in router under `/{tenantID}/pos/online-orders/`:

| Method | Path | Description |
|--------|------|-------------|
| GET | `/online-orders/pickup` | List orders awaiting pickup |
| POST | `/online-orders/{id}/ready` | Mark order ready for collection |
| POST | `/online-orders/{id}/collected` | Mark order collected |

**Handler:** `internal/http/handlers/online_orders.go`  
**Status:** ✅ Pickup endpoints implemented. ❌ NATS subscriber for `ordering.order.status.changed` → KDS ticket creation not yet implemented (see Sprint 13).

---

## 7. Notifications Service Integration

**Used for:**
- KDS waiter-call notifications (`pos.kds.waiter.called` → notifications-service push)
- KDS ticket ready notifications (`pos.kds.ticket.ready`)
- Hotel check-in/check-out confirmations
- Stock alert notifications

**Client:** `internal/modules/notifications/client.go` (planned)  
**Env vars:** `NOTIFICATIONS_SERVICE_URL`, `INTERNAL_SERVICE_KEY`

---

## Event Catalog

### Events Published (Outbox)

| Event | Trigger | Consumers |
|-------|---------|-----------|
| `pos.sale.finalized` | Order completed | inventory-api (backflush), treasury-api (ledger) |
| `pos.drawer.closed` | Cash drawer closed | treasury-api (ledger entry) |
| `pos.menu.updated` | Catalog item changed | ordering-backend (storefront sync) |
| `pos.kds.ticket.ready` | KDS ticket marked ready | notifications-service (waiter push) |
| `pos.kds.waiter.called` | Call waiter triggered | notifications-service (waiter push) |
| `pos.room.checked_in` | Hotel check-in | notifications-service |
| `pos.room.checked_out` | Hotel check-out | notifications-service, treasury-api |
| `pos.return.completed` | Return completed (refund/store credit) | inventory-api (restock into the warehouse the sale drew from), treasury-api (GL backstop, idempotent with the synchronous refund), notifications-service |
| `pos.exchange.completed` | Exchange completed | inventory-api (restock the exchanged-away goods) |
| (restock policy) | `restock` flag on the return/exchange/resync payloads | `false` means the goods were written off: inventory leaves stock unchanged and the treasury refund carries `cost: 0` (no COGS reversal). Decided at completion from OutletSetting.metadata `return_no_restock_reasons` (default damaged, defective, expired) or the manager's per-return choice |
| `pos.return.restock_requested` | Restock resync (`POST /pos/returns/restock/resync`) | inventory-api only, so a retry never re-settles money or re-notifies the customer |
| `pos.daily_closing.completed` | Daily close run | treasury-api (reconciliation) |

### Events Consumed (NATS Subscribers)

| Event | Publisher | Action | Status |
|-------|-----------|--------|--------|
| `inventory.catalog.updated` | inventory-api | Refresh `catalog_items` projection | ❌ Not wired |
| `inventory.stock.low` | inventory-api | Create stock alert notification | ❌ Not wired |
| `inventory.return.restocked` | inventory-api | Record the return's restock outcome in `pos_returns.metadata` (`restock_status`, `restock_lines`, `restock_locations`, `restock_skipped`, `restock_error`). Location means the outlet (branch): pos-api resolves the outlet name from the warehouse's `outlet_id`, falling back to the warehouse name for a shared/HQ warehouse. Shown as the "Returned To" column in the Returns list and Returned Items table, and the Stock card on the return detail page | ✅ Wired 2026-10-02 |
| `treasury.payment.success` | treasury-api | Mark payment succeeded (`pos_payments.status = completed`), complete order | ❌ Not wired — Sprint 6 |
| `treasury.payment.failed` | treasury-api | Mark payment failed (`pos_payments.status = failed`) | ❌ Not wired — Sprint 6 |
| `treasury.etims.invoice_transmitted` | treasury-api | Populate `pos_orders.etims_invoice_number` + `etims_qr_code_url` for receipt display | ❌ Not wired — Sprint 12 |
| `ordering.order.status.changed` | ordering-backend | Create KDS ticket when hospitality order reaches `confirmed`/`preparing` | ❌ Not wired — Sprint 13 |

---

## Environment Variables

```bash
# Single S2S key used for ALL outbound service calls (X-API-Key header)
INTERNAL_SERVICE_KEY=<platform shared S2S key>

# Service URLs
INVENTORY_SERVICE_URL=https://inventoryapi.codevertexafrica.com
TREASURY_SERVICE_URL=https://booksapi.codevertexafrica.com
NOTIFICATIONS_SERVICE_URL=https://notificationsapi.codevertexafrica.com
ORDERING_SERVICE_URL=https://orderingapi.codevertexafrica.com
```

**S2S Auth Standard**: All Codevertex internal services use a single `INTERNAL_SERVICE_KEY` env var. The same key value is sent as `X-API-Key` header on every S2S call regardless of the target service. Each receiving service validates it against its own `INTERNAL_SERVICE_KEY`. There are no per-service API keys.
