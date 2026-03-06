# Sprint 1 -- MVP Foundation

**Timeline**: March 3 - March 17, 2026
**Goal**: Build core POS domain logic (orders, payments, catalog sync, cash management) on top of the existing infrastructure foundation. Ship as part of BengoBox MVP.

---

## Existing foundation (pre-sprint)

- [x] Go project scaffold with Chi router
- [x] Auth middleware (JWT + API key via shared-auth-client)
- [x] Postgres connection pool (pgx)
- [x] Redis client
- [x] NATS connection + `pos` stream setup
- [x] Outbox schema (`migrations/001_outbox_events.sql`)
- [x] Outbox repository (pgx-based)
- [x] User handler (sync, roles)
- [x] RBAC service (in-memory)
- [x] Health endpoints (`/healthz`, `/readyz`, `/metrics`)
- [x] Swagger UI
- [x] CI/CD pipeline (GitHub Actions)
- [x] Production deployment at posapi.codevertexitsolutions.com

---

## Deliverables

### D1: Ent schemas and Atlas migrations (Days 1-2)

- [ ] Scaffold `internal/ent/` with `go generate`
- [ ] Define Ent schemas for MVP tables:
  - `outlets` (id, tenant_id, tenant_slug, code, name, channel_type, status, timezone)
  - `outlet_settings` (outlet_id PK, receipts_json, tax_config_json, service_charge_json, opening_hours_json)
  - `catalog_items` (id, tenant_id, external_item_id, source_service, name, category, barcode, base_price, tax_code, modifier_schema, status, synced_at)
  - `modifier_groups` (id, tenant_id, name, required, min_select, max_select)
  - `modifiers` (id, modifier_group_id, catalog_item_id, label, price_delta)
  - `pos_orders` (id, tenant_id, outlet_id, device_id, order_number, channel, status, order_type, subtotal, discount_total, tax_total, service_charge_total, total_amount, paid_amount, balance_amount, currency, opened_at, closed_at)
  - `pos_order_lines` (id, pos_order_id, catalog_item_id, name_snapshot, quantity, unit_price, discount_amount, tax_amount, notes)
  - `pos_line_modifiers` (id, order_line_id, modifier_id, label_snapshot, price_delta)
  - `pos_order_events` (id, pos_order_id, event_type, payload, actor_user_id, occurred_at)
  - `tenders` (id, tenant_id, name, tender_type, provider_code, is_active)
  - `pos_payments` (id, pos_order_id, tender_id, amount, currency, tip_amount, payment_status, provider_reference, processed_at)
  - `cash_drawers` (id, tenant_id, outlet_id, device_id, opening_user_id, closing_user_id, opening_float, closing_amount, variance_amount, status, opened_at, closed_at)
  - `cash_drawer_events` (id, cash_drawer_id, event_type, amount, performed_by, performed_at, notes)
  - `tables` (id, tenant_id, outlet_id, table_code, area, seat_count, status)
  - `table_assignments` (id, table_id, pos_order_id, assigned_at, released_at)
- [ ] Generate Atlas baseline migration
- [ ] Configure `atlas.hcl` for Ent integration
- [ ] Test migration apply against local and staging DB
- [ ] Remove raw SQL migration reliance (keep `001_outbox_events.sql` as baseline reference)

### D2: Catalog sync (Days 2-3)

- [ ] `internal/modules/catalog/` -- service + repository
- [ ] `GET /api/v1/{t}/pos/catalog/items` -- list items with category filter, search, pagination
- [ ] `GET /api/v1/{t}/pos/catalog/categories` -- list distinct categories
- [ ] NATS subscriber: `inventory.catalog.updated` -- upsert catalog_items
- [ ] NATS subscriber: `inventory.stock.updated` -- update item availability
- [ ] NATS subscriber: `inventory.stock.low` -- flag items as low stock
- [ ] Seed catalog data for `urban-loft` Busia outlet

### D3: Order management (Days 3-6)

- [ ] `internal/modules/order/` -- service + repository
- [ ] `POST /api/v1/{t}/pos/orders` -- create order (with lines and modifiers)
- [ ] `GET /api/v1/{t}/pos/orders` -- list orders (filters: status, outlet, date range, order_type)
- [ ] `GET /api/v1/{t}/pos/orders/{id}` -- order detail with lines, modifiers, payments
- [ ] `PUT /api/v1/{t}/pos/orders/{id}/status` -- status transitions (open -> in_progress -> ready -> completed)
- [ ] `POST /api/v1/{t}/pos/orders/{id}/lines` -- add line item
- [ ] `PUT /api/v1/{t}/pos/orders/{id}/lines/{lineId}` -- update quantity/modifiers
- [ ] `DELETE /api/v1/{t}/pos/orders/{id}/lines/{lineId}` -- remove line
- [ ] Auto-generate order number (sequential per outlet per day: `POS-NNNN`)
- [ ] Calculate totals (subtotal, tax, service charge, total)
- [ ] Outbox event: `pos.order.created`, `pos.order.ready`, `pos.order.completed`
- [ ] Stock consumption event: `pos.stock.consumption` on order completion

### D4: Payment processing (Days 6-8)

- [ ] `internal/modules/payment/` -- service + repository
- [ ] `POST /api/v1/{t}/pos/orders/{id}/payments` -- record payment
- [ ] `GET /api/v1/{t}/pos/orders/{id}/payments` -- list payments for order
- [ ] Tender types: cash (immediate), card (via treasury), mobile_money (via treasury)
- [ ] Cash: calculate change due, mark order completed if fully paid
- [ ] Card/mobile: create payment intent via treasury-api REST, mark `pending`
- [ ] NATS subscriber: `treasury.payment.success` -- mark payment completed
- [ ] NATS subscriber: `treasury.payment.failed` -- mark payment failed
- [ ] Split payment support (multiple tenders per order)
- [ ] Outbox event: `pos.payment.initiated`

### D5: Cash drawer management (Days 8-9)

- [ ] `internal/modules/drawer/` -- service + repository
- [ ] `POST /api/v1/{t}/pos/drawers/open` -- open drawer with float
- [ ] `POST /api/v1/{t}/pos/drawers/close` -- close drawer, calculate variance
- [ ] `GET /api/v1/{t}/pos/drawers/current` -- get current open drawer for outlet/device
- [ ] Drawer events: open, close, skim, drop, no_sale
- [ ] Variance alert: publish `pos.cash.drawer.alert` if variance exceeds threshold

### D6: Table management (Days 9-10)

- [ ] `GET /api/v1/{t}/pos/tables` -- list tables for outlet
- [ ] `POST /api/v1/{t}/pos/tables/{id}/assign` -- assign order to table
- [ ] `POST /api/v1/{t}/pos/tables/{id}/release` -- release table
- [ ] Table status: available, occupied, reserved, dirty

### D7: Event wiring and outbox (Days 10-11)

- [ ] Wire outbox publisher in `app.go` (start background goroutine)
- [ ] Wire all NATS subscribers (auth, inventory, treasury, logistics)
- [ ] Test event flow: order created -> stock consumed -> payment processed
- [ ] DLQ handling for failed events

### D8: Testing and deploy (Days 11-12)

- [ ] Integration tests for order lifecycle
- [ ] Integration tests for payment flow (cash + treasury mock)
- [ ] Integration tests for cash drawer open/close
- [ ] Load test: 50 concurrent order creations
- [ ] Deploy to production
- [ ] Smoke test with pos-ui

---

## Risks and mitigations

| Risk | Impact | Mitigation |
|------|--------|-----------|
| Ent schema complexity (16+ tables) | Slow D1 | Start with core 8 tables, defer promotions/gift cards |
| Treasury API not ready for card payments | Payment flow incomplete | Cash-only for MVP; card/mobile returns 501 |
| Inventory catalog not seeded | Empty menu in POS | Seed catalog items directly in pos-api DB |
| NATS subscribers untested with real events | Events dropped | Manual event publishing for testing |

---

## Definition of done

- [ ] All D1-D7 deliverables functional
- [ ] Orders can be created, updated, and completed via API
- [ ] Cash payments processed with correct change calculation
- [ ] Cash drawer open/close with variance calculation
- [ ] Catalog items queryable by category and search
- [ ] Outbox publishing events to NATS
- [ ] Atlas migrations running in CI
- [ ] Deployed to posapi.codevertexitsolutions.com
- [ ] No 500 errors on happy-path flows
