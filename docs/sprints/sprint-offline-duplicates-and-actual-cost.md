# Offline duplicate bills and actual sale cost (2026-11-26)

## Why

Urban Loft reported a dine-in bill rung up twice during an internet outage. Order 003392 (1x Weekend Platter, 4,790) went to the kitchen and was served; order 003390 was the bill the guest actually paid. 003392 could not be voided from the till because it had been served.

The owner also asked what happens to the buying price when an item is restocked at a new cost (5 units bought at 50, restocked at 80 while 3 old units remain), and how old and new stock are costed in POS, reports and the ledger.

## What I found

Duplicate bills:

1. An offline order dropped its table, covers, discount, charges and served-by when it was queued, so after sync the bill sat on no table. The till also assigned the table under the order's local id, which matches no server order, so the Tables screen showed no bill and no "+ Add to bill". The waiter rang the order up again.
2. The Tables list loaded table assignments with `.Limit(1)` inside `WithAssignments`. In ent that limit applies to the whole eager query, not per table, so only one table in the outlet ever showed its open bill.
3. Every send minted a new `client_reference`, so a retry, a second tap or a reload mid-request could create a second bill.
4. Adding items to a bill had no Idempotency-Key, no per-line id and no offline queue. A lost reply plus a retry doubled the lines; a hard failure pushed the waiter to start a new bill.
5. Nothing stopped a second open bill being started on a table that already had one.
6. The Void button hid itself for `pending_payment`, the status a bill takes once the kitchen marks every ticket served. pos-api always accepted that void. A void also never released the table.

Cost:

- inventory-api already keeps a cost layer per receipt and draws stock lot by lot (FIFO, LIFO, FEFO, or the running average for wavg).
- POS and the treasury COGS entry used the item's current cost, not what inventory charged. Under FIFO the 3 old units sold after a restock at 80 were costed at 80 in POS reports and the ledger, while inventory charged 50. The ledger's inventory balance drifted away from inventory's own stock value.
- Under wavg (the default) inventory blends every open layer to the running average on each receipt. That is correct perpetual average costing; the original receipt cost stays on the goods receipt line. The receipt dialog wrongly said stock on hand keeps its old cost; I fixed the wording.

## What I changed

pos-api:
- New `tableseat` module owns seating: `Seat` (idempotent), `ReleaseOrder` (keeps the table occupied while another bill sits there), `OpenOrderOnTable`, `LiveOrderIDs` (two queries for the whole tables page) and the guard.
- `CreateOrder` seats the order at its table on the server, so online and synced offline orders behave the same.
- Table guard from outlet settings metadata `table_guard`: `warn` (default), `block` or `off`. In warn mode a second bill needs `metadata.separate_bill=true`. The answer is 409 `table_has_open_order` with the existing bill. A sale synced from an offline queue is never refused; it is seated and flagged with `table_conflict_order_id`.
- `AssignTable` checks the order exists in the tenant and is idempotent.
- `AddOrderLines` skips lines whose `metadata.client_line_id` is already on the bill.
- The Idempotency-Key middleware no longer stores 401, 403, 409, 422, 428 or 429. The till keeps one key per sale attempt, so a resend after manager approval or a "separate bill" choice must reach the handler again.
- Void reasons are configurable per outlet (metadata `void_reasons`: code, label, record_wastage) with defaults: duplicate, entered in error, customer cancelled before preparation, prepared then refused. `GET /pos/void-reasons` serves them. A void with a `record_wastage` reason consumes the bill's unvoided items from stock with reason `pos_void_wastage` (idempotent on the order). Every void releases the table. `duplicate_of` is recorded on the order.
- New consumer of `inventory.consumption.costed`: replaces each sold line's `unit_cost_at_sale` with inventory's actual cost and sets `cost_source=inventory` (the till's estimate is kept as `unit_cost_estimate`). Every profit report already reads the snapshot first, so no report changed.
- The inventory event consumer now waits before redelivering a failed event (2s doubling to 2 minutes, with jitter, bounded by MaxDeliver) and drops unparsable events instead of redelivering them.

pos-ui:
- One attempt key per cart (`saleAttemptKey`), saved in the sale-session snapshot, reused on retry and cleared after success or a definite refusal.
- Offline orders keep table, covers, discount, charges, served-by, promotion, metadata and approval; the sync sends them all.
- An in-flight 409 queues the sale under the same key instead of erroring.
- Add-to-bill sends a key and per-line `client_line_id`, and queues offline (new `offlineLineAdds` store, Dexie v8). A bill still queued offline gets the items appended to it. The drain runs after orders, with the shared backoff and dead-letter rules.
- Tables shows queued offline bills ("Offline XXXXXX", "Pending sync") so the waiter adds to them.
- Table conflict prompt: "Add to that bill" or, in warn mode, "Separate bill".
- Void dialog lists the outlet's reasons and marks the ones that record wastage. Void Bill shows for `pending_payment`.

inventory-api:
- `RecordConsumption` publishes `inventory.consumption.costed` (order, reason, costing method, per sold SKU total cost) in the same transaction, and returns `costs` on the S2S response.

treasury-api:
- New `COGSTrueUpSubscriber`: on `inventory.consumption.costed` for a POS sale, posts the difference between inventory's cost and the sale's COGS entry (reference `cogs_trueup`, once per sale). It waits for the sale's own COGS with backoff for up to 8 deliveries; a sale whose COGS was reversed is skipped; a sale that never posted COGS gets the full actual cost on the last attempt.

inventory-ui:
- Goods receipt shows current cost, this receipt's cost, the resulting cost by costing method and the margin. Item data is fetched per PO line instead of a 500-item page.
- Lots list shows each batch's unit cost.

## Deploy order

1. pos-api (accepts the new fields and the new event; the till keeps working with an older server).
2. inventory-api (starts publishing `inventory.consumption.costed`).
3. treasury-api.
4. pos-ui, inventory-ui.

No schema migration. Everything new lives in metadata or JSON payloads.

## Still open

- Returns, reversals and sale edits still restock at the item's current cost and do not restore the original lot.
- Transfers, relocations, bulk adjust destinations, direct job purchases, opening stock and return restocks create no cost layer.
- A receipt that changes an ingredient's cost does not recost the recipes that use it.
- `ConsumeReservation` writes no costed consumption line.
- `EnhancedSummary.total_inventory_value` is always 0. No screen shows it yet; fixing it needs an SQL aggregate, not the in-memory valuation.
- Offline kitchen chits still print a short local id; the Sync Monitor does not yet map it to the server number.
- The Playwright offline spec does not cover dine-in add-to-bill yet.
- Order 003392 at Urban Loft: a manager voids it from Bills with reason "Duplicate order" once pos-ui is deployed.
