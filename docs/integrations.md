# POS Service - Integration Guide

## Overview
The POS service is the **Source of Truth for Sales Catalogs (Menus)**. While `inventory-api` owns the physical item (Product Master), `pos-api` owns how those items are grouped, priced, and displayed for sale at an outlet.

## Core Integration Points

### 1. Inventory Service (Product Master)
- **Catalog Authoring**: When a user creates a "Menu Item" in POS, the POS service:
  - Verifies/Creates the `Product Master` in `inventory-api`.
  - Stores a local `catalog_item` as a projection.
  - Links modifiers and POS-specific pricing to the item.
- **Stock Depletion (Backflushing)**: Upon `SaleFinalized`, POS publishes an outbox event that `inventory-api` consumes to decrement raw ingredients based on the Recipe/BOM.

### 2. Ordering Backend (Fulfillment)
- **Catalog Sync**: POS publishes `pos.menu.updated` events. `ordering-backend` consumes these to update its online storefront projection.
- **Order Handoff**: Online-for-pickup orders are initiated in Ordering and handed off to POS for fulfillment and kitchen printing.

### 3. Treasury Service (Payments)
- **Payment Processing**: POS uses `treasury-api` to process card, mobile money (M-Pesa), and cash transactions.
- **Cash Management**: Cash drawer events (skims, drops) are reported to Treasury for financial auditing.

## Event Catalog
- `pos.menu.updated`: Published when categories, prices, or availability change.
- `pos.sale.finalized`: Triggers inventory backflush and accounting ledger updates.
- `pos.drawer.closed`: Reports end-of-shift cash positions.
