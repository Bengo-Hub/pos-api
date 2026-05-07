# Sprint 10: Loyalty & Advanced Promotions — pos-api

**Status:** 🔴 Not Started  
**Period:** October–November 2026  
**Goal:** Customer loyalty points, tiered membership, advanced promotion engine (multi-buy, combo, time-based discounts)

---

## Context

Basic promotions (percentage discount, fixed discount) already exist via the `Promotion` and `PromotionRule` schemas. This sprint extends the engine to support more complex real-world scenarios:
- Buy-X-get-Y (BXGY) promotions common in retail
- Combo meals / bundle pricing (restaurant, fast food)
- Time-based happy hour pricing
- Customer loyalty points that accumulate across visits and can be redeemed as tender
- Tiered membership (Bronze/Silver/Gold) that unlocks different price books or discount rates
- Referral tracking

---

## Deliverables

### Loyalty Points Engine
- [ ] `LoyaltyAccount` schema — `id, tenant_id, client_phone (string, indexed), client_name, points_balance (int), lifetime_points (int), tier enum(standard|bronze|silver|gold), tier_updated_at, created_at, updated_at`
- [ ] `LoyaltyTransaction` schema — `id, tenant_id, loyalty_account_id (FK), pos_order_id (FK nullable), type enum(earn|redeem|expire|adjust|bonus), points (int), balance_after (int), description, created_by, created_at`
- [ ] `LoyaltyConfig` schema — `id, tenant_id, earn_rate (int — points per currency unit, e.g. 1 point per KES 10), redeem_rate (int — points per currency unit, e.g. 100 points = KES 10), min_redeem_points (int), expiry_days (int nullable), tier_thresholds (JSON: {bronze, silver, gold}), is_active`
- [ ] `GET /{tenant}/pos/loyalty/config` — get loyalty config
- [ ] `PUT /{tenant}/pos/loyalty/config` — create or update loyalty config
- [ ] `GET /{tenant}/pos/loyalty/accounts?phone={phone}` — look up loyalty account
- [ ] `POST /{tenant}/pos/loyalty/accounts` — create account (or return existing by phone)
- [ ] `GET /{tenant}/pos/loyalty/accounts/{id}/transactions` — transaction history
- [ ] Points earning: auto-triggered on `pos.sale.finalized` via order service — calculates points based on net sale amount and creates `LoyaltyTransaction(type=earn)`
- [ ] Points redemption: new tender type `loyalty_points` — `POST /{tenant}/pos/orders/{id}/loyalty/redeem` — validates balance, deducts points, applies as tender
- [ ] Tier recalculation: after each earn, check `lifetime_points` against tier thresholds; auto-promote/demote tier
- [ ] Loyalty bonus rules: configurable per catalog_item or category (e.g., double points on Tuesdays)

### Advanced Promotion Engine Extensions
- [ ] New rule type: `buy_x_get_y` — buy N of item A, get M of item B free/discounted
  - `PromotionRule.rule_type` enum: add `buy_x_get_y`
  - New fields: `buy_qty (int)`, `buy_item_id (FK nullable — null = any item)`, `get_qty (int)`, `get_item_id (FK nullable)`, `get_discount_pct (float)`
- [ ] New rule type: `combo` — list of items that together qualify for a bundle price
  - `PromotionRule.rule_type`: add `combo`
  - New fields: `combo_items (JSON: [{catalog_item_id, qty}])`, `combo_price (float)`
- [ ] New rule type: `time_window` — active only during specific hours/days
  - `PromotionRule.valid_from_time (string HH:MM nullable)`, `valid_to_time (string HH:MM nullable)`, `valid_days (JSON: [0-6] — 0=Sunday)`
- [ ] Promotion evaluation order: promotions are applied in priority order; first match wins unless `allow_stacking = true`
- [ ] `POST /{tenant}/pos/promotions/evaluate` — dry-run promotion calculation for a cart (used by pos-ui before order creation)

### Referral Tracking
- [ ] `ReferralRecord` schema — `id, tenant_id, referrer_phone, referred_phone, pos_order_id (FK — qualifying order), reward_type enum(points|discount|gift), reward_value, status (pending|issued|cancelled), created_at`
- [ ] `POST /{tenant}/pos/loyalty/referrals` — record a referral (referrer phone + new client phone)
- [ ] On new client's first qualifying order: auto-issue reward to referrer loyalty account

### Price Book Integration with Tiers
- [ ] Loyalty tier → PriceBook linkage: `loyalty_tier_pricebook_map (JSON: {bronze: pricebook_id, silver: ..., gold: ...})` in `LoyaltyConfig`
- [ ] On cart evaluation: if client has loyalty account, apply tier-specific PriceBook prices automatically

### RBAC Additions
- [ ] New permissions: `pos.loyalty.view`, `pos.loyalty.change`, `pos.loyalty.manage`
- [ ] New permissions: `pos.promotions.manage` (extends existing)
- [ ] Assign `pos.loyalty.view` to `cashier`; `pos.loyalty.manage` to `store_manager`, `pos_admin`

### Migration
- [ ] Add `LoyaltyAccount`, `LoyaltyTransaction`, `LoyaltyConfig` ent schemas
- [ ] Add `ReferralRecord` ent schema
- [ ] Extend `PromotionRule`: add `buy_x_get_y`, `combo`, `time_window` rule types and fields
- [ ] Add `valid_from_time`, `valid_to_time`, `valid_days`, `allow_stacking` to `Promotion`
- [ ] Add tender type `loyalty_points` to `Tender` seed
- [ ] Run `go generate ./internal/ent`
- [ ] Generate Atlas migration: `loyalty_promotions`
- [ ] Update `docs/erd.md`

---

## Use Cases Covered

| Use Case | Business Types |
|----------|---------------|
| Earn points on every purchase | All retail, hospitality, service |
| Redeem points as payment tender | All business types |
| Tiered membership (Bronze/Silver/Gold) | Supermarket, pharmacy, hotel |
| Buy-2-get-1-free promotions | Supermarket, retail, restaurant |
| Combo/bundle pricing | Restaurant, fast food, retail |
| Happy hour / time-window discounts | Restaurant, bar, hotel |
| Referral rewards | Salon, pharmacy, retail |
| Tier-based pricing | Hotel, supermarket loyalty clubs |
