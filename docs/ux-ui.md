# pos-api -- UX/UI Contracts

**Purpose**: Define the API response shapes and behaviour that pos-ui depends on for order entry, payments, and cash management.

---

## Consumer application

| Client | Auth | Primary use cases |
|--------|------|-------------------|
| pos-ui (Next.js PWA) | JWT (cashier/supervisor/manager) | Order entry, payment, cash drawer, table management |

---

## Key response contracts

### Order (POST /api/v1/{tenantID}/pos/orders)

Request:
```json
{
  "outlet_id": "uuid",
  "order_type": "dine_in",
  "table_id": "uuid",
  "lines": [
    {
      "catalog_item_id": "uuid",
      "quantity": 2,
      "unit_price": 450.00,
      "notes": "No onions",
      "modifiers": [
        { "modifier_id": "uuid", "price_delta": 50.00 }
      ]
    }
  ]
}
```

Response:
```json
{
  "id": "uuid",
  "order_number": "POS-0042",
  "outlet_id": "uuid",
  "order_type": "dine_in",
  "status": "open",
  "table_id": "uuid",
  "subtotal": 950.00,
  "discount_total": 0.00,
  "tax_total": 152.00,
  "service_charge_total": 95.00,
  "total_amount": 1197.00,
  "currency": "KES",
  "lines": [...],
  "opened_at": "2026-03-10T14:00:00Z"
}
```

### Catalog items (GET /api/v1/{tenantID}/pos/catalog/items)

Query params: `category`, `search`, `available_only` (default true).

```json
{
  "data": [
    {
      "id": "uuid",
      "name": "Chicken Burger",
      "category": "burgers",
      "barcode": "123456789",
      "base_price": 450.00,
      "tax_code": "VAT_16",
      "available": true,
      "modifier_groups": [
        {
          "id": "uuid",
          "name": "Extras",
          "required": false,
          "min_select": 0,
          "max_select": 3,
          "modifiers": [
            { "id": "uuid", "label": "Extra Cheese", "price_delta": 50.00 },
            { "id": "uuid", "label": "Bacon", "price_delta": 100.00 }
          ]
        }
      ],
      "image_url": "https://..."
    }
  ],
  "meta": { "page": 1, "per_page": 50, "total": 120 }
}
```

### Payment (POST /api/v1/{tenantID}/pos/orders/{orderId}/payments)

Request:
```json
{
  "tender_type": "cash",
  "amount": 1200.00,
  "tip_amount": 3.00
}
```

Response:
```json
{
  "id": "uuid",
  "pos_order_id": "uuid",
  "tender_type": "cash",
  "amount": 1200.00,
  "tip_amount": 3.00,
  "change_due": 3.00,
  "payment_status": "completed",
  "processed_at": "2026-03-10T14:05:00Z"
}
```

For card/mobile money, `payment_status` may be `pending` until treasury webhook confirms.

### Cash drawer (POST /api/v1/{tenantID}/pos/drawers/open)

Request:
```json
{
  "outlet_id": "uuid",
  "device_id": "uuid",
  "opening_float": 5000.00
}
```

Response:
```json
{
  "id": "uuid",
  "outlet_id": "uuid",
  "device_id": "uuid",
  "opening_float": 5000.00,
  "status": "open",
  "opened_at": "2026-03-10T08:00:00Z"
}
```

### Cash drawer close (POST /api/v1/{tenantID}/pos/drawers/close)

Request:
```json
{
  "closing_amount": 15200.00,
  "notes": "All cash accounted for"
}
```

Response:
```json
{
  "id": "uuid",
  "opening_float": 5000.00,
  "closing_amount": 15200.00,
  "expected_amount": 15350.00,
  "variance_amount": -150.00,
  "status": "closed",
  "closed_at": "2026-03-10T22:00:00Z"
}
```

### Tables (GET /api/v1/{tenantID}/pos/tables)

```json
{
  "data": [
    {
      "id": "uuid",
      "table_code": "T-01",
      "area": "Main Floor",
      "seat_count": 4,
      "status": "occupied",
      "current_order_id": "uuid"
    }
  ]
}
```

---

## Order status flow

```
open -> in_progress -> ready -> completed
  \                             /
   +-------> cancelled --------+
   +-------> voided
```

| Status | Meaning |
|--------|---------|
| `open` | Order created, lines being added |
| `in_progress` | Sent to kitchen |
| `ready` | Kitchen ready, awaiting serve/pickup |
| `completed` | Fully paid and closed |
| `cancelled` | Cancelled before completion |
| `voided` | Voided by supervisor (post-serve) |

---

## Error response format

```json
{
  "error": {
    "code": "INSUFFICIENT_STOCK",
    "message": "Item X has only 2 units available",
    "details": { "item_id": "uuid", "available": 2, "requested": 5 }
  }
}
```

HTTP status codes: 400 (validation), 401 (unauthenticated), 403 (unauthorized), 404 (not found), 409 (conflict), 422 (business rule), 500 (internal), 501 (not implemented).

---

## Pagination

All list endpoints: `{ "data": [...], "meta": { "page", "per_page", "total" } }`.

Default `per_page`: 20 (50 for catalog items), max: 100.

---

## Tenant header requirements

| Header | Required |
|--------|----------|
| `Authorization: Bearer {jwt}` | Yes |
| `X-Tenant-Slug` | Recommended |
| `X-Outlet-ID` | Required for order/drawer/table endpoints |

The `{tenantID}` path parameter is the primary tenant discriminator. `X-Outlet-ID` scopes operations to the active outlet (Busia for MVP).
