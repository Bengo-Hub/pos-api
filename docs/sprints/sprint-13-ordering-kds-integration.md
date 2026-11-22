# Sprint 13: Online Ordering → KDS Integration — pos-api

**Status:** 🟡 Partial — online order pickup endpoints, KDSSyncFailure DLQ schema, table_reference field, and kds.order.ready event shipped; NATS subscriber for automatic KDS ticket creation not implemented  
**Period:** January–February 2027  
**Last updated:** 2026-05-25  
**Goal:** Subscribe to ordering-backend events and create KDS tickets when online hospitality orders reach kitchen-ready status, closing the gap between the online ordering channel and the kitchen display system

---

## Context

When a customer places a dine-in or pickup order through the Codevertex ordering app (or a third-party channel like Uber Eats), the order flows through ordering-backend. The kitchen has no visibility of this order in the KDS until a waiter manually enters it in the POS terminal — an unacceptable gap for hospitality businesses.

This sprint wires the NATS event bridge so that when ordering-backend transitions an order to `confirmed` or `preparing`, pos-api automatically creates KDS tickets, routing line items to the correct kitchen station (kitchen, bar, grill) exactly as a manually-entered POS order would.

---

## Deliverables

### NATS Subscriber

- [ ] Subscribe to `ordering.order.status.changed` on NATS JetStream (consumer group: `pos-api-kds`)
- [ ] Filter: `new_status IN (confirmed, preparing)` AND `fulfillment_type IN (dine_in, pickup)`
- [ ] Ignore `fulfillment_type = delivery` (logistics-only flow)
- [ ] Idempotent: skip if a KDS ticket already exists for `external_order_id`

### KDS Ticket Creation

On qualifying event:
- [ ] Lookup or create a `PosOrder` with:
  - `order_source = online`
  - `external_order_id = ordering_order_id`
  - `order_subtype = dine_in` or `takeaway` (mapped from `fulfillment_type`)
  - `table_id` populated if `table_reference` present in event payload
- [ ] For each `OrderItem` in the event:
  - Create `PosOrderLine` with `kds_status = sent`, `kds_sent_at = now()`
  - Route to station based on `catalog_item.kds_station` (kitchen|bar|grill); default = kitchen
  - Preserve item notes and modifiers
- [ ] Create `KDSTicket` aggregate per station (or per order line — match existing KDS ticket model)
- [ ] Publish `pos.kds.ticket.created` outbox event

### Completion Callback (optional — Phase 2)

- [x] `pos.kds.order.ready` event published when a KDS ticket transitions to `ready` status — payload: `{ tenant_id, order_id, station_id, ticket_id }`
- [ ] ordering-backend subscription to auto-transition ordering order to `ready` — pending ordering-backend implementation
- [ ] Feature flag `ENABLE_KDS_ORDERING_CALLBACK` — not yet added (event always fires on ready transition)

### Table Matching

- [x] `table_reference` field added to `KDSTicket` Ent schema — stores the raw table label string from the ordering event payload (e.g. "Table 7"); nullable
- [ ] Lookup `Table` by `label` within the same outlet and assign `PosOrder.table_id` — lookup logic not yet implemented
- [ ] If `fulfillment_type = pickup`: no table assignment; order shows in KDS as "Pickup — #{queue_number}"

### Error Handling

- [x] Dead-letter queue schema: `KDSSyncFailure` Ent entity (`internal/ent/schema/kdssyncfailure.go`) — `id`, `tenant_id`, `external_order_id`, `error_message`, `raw_payload` (JSON), `retry_count`, `resolved_at`, `created_at`
- [ ] DLQ consumer: route failed events to `KDSSyncFailure` after 3 retries (schema exists; consumer not wired)
- [ ] Alert via notifications-service if DLQ exceeds 5 entries within 10 minutes

### RBAC

No new permissions needed — KDS ticket creation is an internal system action, not user-initiated.

### Migration

- [ ] Add `external_order_id` (string, nullable) to `pos_orders` if not already present
- [ ] Add index on `(tenant_id, external_order_id)` for idempotency check
- [ ] Generate Atlas migration: `online_order_kds_integration`

---

## Event Payload Contract

Expected shape of `ordering.order.status.changed` from ordering-backend:

```json
{
  "order_id": "uuid",
  "tenant_id": "uuid",
  "outlet_id": "uuid",
  "previous_status": "pending",
  "new_status": "confirmed",
  "fulfillment_type": "dine_in",
  "table_reference": "Table 7",
  "items": [
    {
      "id": "uuid",
      "sku": "ITEM-001",
      "name": "Grilled Chicken",
      "quantity": 2,
      "unit_price": 850,
      "notes": "no onions",
      "modifiers": ["extra sauce"]
    }
  ],
  "occurred_at": "2026-01-15T18:30:00Z"
}
```

> If ordering-backend does not yet include `items` in the status change event, a follow-up REST call to `GET /api/v1/orders/{order_id}` is made by pos-api to fetch line details. See ordering-backend integrations.md for the S2S endpoint.

---

## Implementation Notes

- Subscriber lives in `internal/platform/events/subscribers/ordering_kds.go`
- KDS ticket creation logic reuses `internal/modules/kds/service.go` — the same code path used by the waiter-facing `POST /{tenant}/pos/kds/tickets` endpoint
- Station routing: `internal/modules/kds/station_router.go` maps item category to KDS station
- The `external_order_id` field ensures a re-delivered NATS event does not create duplicate tickets (idempotency key)

---

## Day view and shift tally (2026-11-22)

Urban Loft asked for served orders to stay on the board, crossed out, so baristas and chefs can count what they sold against the system when they close a shift. Before this, the board listed only pending, in progress and ready tickets from the last 24 hours, so a served ticket vanished the moment it was bumped and the only record left was the sales report.

How tickets are handled now:

- Tickets are created per station when a sale or online order reaches the kitchen (`orders` routing through `kdsroute`) and move pending, in progress, ready, served, or voided. `POST .../kds/tickets/clear` serves every active ticket of the outlet in one tap. Served and voided rows were always kept in `kds_tickets`; only the board hid them.
- `GET .../pos/kds/tickets` keeps its live behaviour by default. With `view=day`, `date=YYYY-MM-DD` or `hour=0..23` it returns every ticket received in that tenant-local day or hour, all statuses, oldest first, capped by `limit` (default 500, max 2000) with `truncated` set when more exist. The day is the tenant's calendar day from the local tenants projection (Africa/Nairobi by default), never the pod's UTC day.
- `GET .../pos/kds/tickets/day-summary?date=&hour=&station_id=` (`kds.BuildDaySummary`) returns all 24 hourly buckets (tickets, open, served, voided, item quantities) and the item tally of the day or the chosen hour. It runs two grouped SQL reads on the read replica, so its cost follows the number of hours and distinct items, not the number of tickets. Every value is a bound parameter.
- Index `kdsticket_day_window (tenant_id, received_at)` (migration `20261122120000`) serves both reads; the partial `kdsticket_live_board` index still serves the live board.
- Outlet settings, in `OutletSetting.metadata` with no new column: `kds_default_view` (`day` by default, or `live`), `kds_keep_served` (default true) and `kds_refresh_seconds` (60 to 300, default 60, clamped by `kds.NormalizeRefreshSeconds`). All go through `PUT .../pos/settings` and show on the KDS Stations settings tab.
- Screens reload in place. The board listens on `GET .../pos/kds/stream` (the `ticket_created` and `ticket_updated` pushes refetch the list once, debounced) and polls only every `kds_refresh_seconds` as the fallback. Earlier the board polled every 5 seconds, and every minute the whole page went blank because each mounted `useSubscription` reset the entitlements to loading and fired its own `/api/subscription` request; pos-ui now runs one shared lookup at a time and re-checks in the background.

The pos-ui board opens on the day view: an hour strip with all 24 hours (quiet hours muted, All hours selected by default, tap an hour to drill down), an item tally table (served, open, voided, total) for the station being viewed, and the ticket cards. Finished cards sit after the live rail with an X across them, struck-through items and their frozen prep time. Every elapsed timer in pos-ui (KDS cards, table occupancy, open sales on the sales list) now reads `YYYY:MM:DD:HH:mm:ss` from one helper, `src/lib/time/elapsed.ts`.

---

## Partial Implementation (updated 2026-05-25)

### Live Endpoints (`online_orders.go` handler)
- [x] `GET /{tenant}/pos/online-orders/pickup` — list pickup orders
- [x] `POST /{tenant}/pos/online-orders/{orderID}/ready` — mark order ready for collection
- [x] `POST /{tenant}/pos/online-orders/{orderID}/collected` — mark order collected

### Schema / Event Additions (2026-05-25)
- [x] `KDSSyncFailure` Ent schema (`internal/ent/schema/kdssyncfailure.go`) — DLQ entity for failed KDS sync events
- [x] `table_reference` field on `KDSTicket` Ent schema — stores raw table label from ordering event payload
- [x] `pos.kds.order.ready` NATS event published when ticket transitions to `ready` status

### Still Unimplemented
- [ ] NATS subscriber for `ordering.order.status.changed` — no subscriber exists
- [ ] Automatic KDS ticket creation from online order events
- [ ] DLQ consumer wiring — schema exists but events are not routed to `KDSSyncFailure` on failure
- [ ] Table lookup from `table_reference` → `PosOrder.table_id`

## Testing

- [ ] Unit test: station routing for kitchen/bar/grill items
- [ ] Unit test: idempotency — duplicate event does not create second ticket
- [ ] Integration test: publish mock `ordering.order.status.changed` event → verify KDS ticket created
- [ ] Integration test: `fulfillment_type = delivery` event → no KDS ticket created
- [ ] Manual: place order in ordering app → confirm ticket appears on pos-ui KDS terminal without manual entry

---

## Use Cases Covered

| Use Case | Business Types |
|----------|---------------|
| Online dine-in order → kitchen display | Restaurant, hotel dining room |
| Online pickup order → kitchen queue | Restaurant, fast food, café |
| Third-party channel (Uber Eats) → KDS | Restaurant, fast food |
| Table context preserved from online order | Restaurant with floor plan integration |
