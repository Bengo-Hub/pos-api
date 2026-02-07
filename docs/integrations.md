# POS Service - Integration Guide

## Overview

This document provides detailed integration information for all external services and systems integrated with the POS service.

---

## Table of Contents

1. [Internal BengoBox Service Integrations](#internal-bengobox-service-integrations)
2. [External Third-Party Integrations](#external-third-party-integrations)
3. [Integration Patterns](#integration-patterns)
4. [Two-Tier Configuration Management](#two-tier-configuration-management)
5. [Event-Driven Architecture](#event-driven-architecture)
6. [Integration Security](#integration-security)
7. [Error Handling & Resilience](#error-handling--resilience)

---

## Internal BengoBox Service Integrations

### Auth Service

**Integration Type**: OAuth2/OIDC + Events + REST

**Use Cases**:
- User authentication and authorization
- JWT token validation
- User identity synchronization
- Tenant/outlet discovery

**Architecture**:
- Uses `shared/auth-client` v0.1.0 library for JWT validation
- All protected `/v1/{tenant}` routes require valid Bearer tokens

**Events Consumed**:
- `auth.tenant.created` - Initialize tenant in POS system
- `auth.tenant.updated` - Update tenant metadata
- `auth.outlet.created` - Create outlet reference
- `auth.outlet.updated` - Update outlet metadata

### Inventory Service

**Integration Type**: REST API + Events (NATS)

**Use Cases**:
- Catalog sync (read-only cache)
- Real-time stock availability
- Stock consumption on sale
- Stock adjustment requests

**REST API Usage**:
- `GET /v1/{tenant}/inventory/items` - Get catalog items
- `GET /v1/{tenant}/inventory/items/{sku}` - Get stock availability
- `POST /v1/{tenant}/inventory/consumption` - Record sales consumption
- `POST /v1/{tenant}/inventory/adjustments` - Request stock adjustment

**Events Consumed**:
- `inventory.catalog.updated` - Update catalog cache
- `inventory.stock.updated` - Update stock availability
- `inventory.stock.low` - Low stock warning

**Events Published**:
- `pos.order.completed` - Consume stock
- `pos.stock.adjustment.requested` - Request adjustment

**Data Ownership**:
- POS maintains read-only catalog cache
- No inventory balances stored locally
- All stock queries go through inventory-service APIs

### Treasury App

**Integration Type**: REST API + Events (NATS) + Webhooks

**Use Cases**:
- Payment processing
- Settlement reconciliation
- Refunds
- Gift card processing

**REST API Usage**:
- `POST /api/v1/payments/intents` - Create payment intent
- `POST /api/v1/payments/confirm` - Confirm payment
- `POST /api/v1/payments/refund` - Process refund
- `GET /api/v1/settlements` - Get settlement data

**Webhooks Consumed**:
- `treasury.payment.success` - Update order payment status
- `treasury.payment.failed` - Handle payment failure
- `treasury.settlement.generated` - Process settlement

**Events Published**:
- `pos.payment.initiated` - Payment intent created
- `pos.settlement.requested` - Settlement request

### Logistics Service

**Integration Type**: REST API + Events (NATS)

**Use Cases**:
- Pickup task creation
- Order handoff confirmation
- Driver pickup tracking

**REST API Usage**:
- `POST /v1/{tenant}/tasks` - Create pickup task
- `GET /v1/{tenant}/tasks/{id}` - Get task status

**Events Consumed**:
- `logistics.task.assigned` - Driver assigned
- `logistics.task.completed` - Pickup completed

**Events Published**:
- `pos.order.ready` - Order ready for pickup
- `pos.order.handoff` - Order handed off to driver

### Notifications Service

**Integration Type**: Events (NATS) + REST API

**Use Cases**:
- Order ready notifications
- Low stock alerts
- Cash drawer alerts

**REST API Usage**:
- `POST /v1/{tenantId}/notifications/messages` - Send notification

**Events Published**:
- `pos.order.ready` - Order ready notification
- `pos.stock.low` - Low stock alert
- `pos.cash.drawer.alert` - Cash drawer alert

### Cafe Backend

**Integration Type**: Events (NATS) + REST API

**Use Cases**:
- Loyalty integration
- Menu sync coordination

**Events Consumed**:
- `cafe.loyalty.points_awarded` - Award loyalty points
- `cafe.menu.updated` - Trigger catalog sync

**Events Published**:
- `pos.loyalty.redemption` - Loyalty points redeemed

---

## External Third-Party Integrations

### Fiscal Printers (Future)

**Purpose**: Fiscal receipt printing for tax compliance

**Configuration** (Tier 1):
- Printer API credentials: Stored encrypted
- Printer endpoints: Stored encrypted

**Status**: Planned for future implementation

### Barcode Scanners

**Purpose**: Barcode scanning for item lookup

**Configuration** (Tier 2):
- Scanner device configuration
- Barcode mapping rules

**Use Cases**:
- Item lookup by barcode
- PLU code scanning

---

## Integration Patterns

### 1. REST API Pattern (Synchronous)

**Use Case**: Immediate stock queries, payment processing

**Implementation**:
- HTTP client with retry logic
- Circuit breaker pattern
- Request timeout (5 seconds default)
- Idempotency keys for mutations

### 2. Event-Driven Pattern (Asynchronous)

**Use Case**: Order events, stock updates, payment callbacks

**Transport**: NATS JetStream

**Flow**:
1. Service publishes event to NATS
2. Subscriber services consume event
3. Process event and update local state
4. Publish response events if needed

**Reliability**:
- At-least-once delivery
- Event deduplication via event_id
- Retry on failure
- Dead letter queue for failed events

### 3. Webhook Pattern (Callbacks)

**Use Case**: Payment status, settlement notifications

**Implementation**:
- Webhook endpoints in POS service
- Signature verification (HMAC-SHA256)
- Retry logic for failed deliveries
- Idempotency handling

### 4. Offline Queue Pattern

**Use Case**: Operations during network outages

**Implementation**:
- Redis queue for offline operations
- Automatic sync when connectivity restored
- Conflict resolution strategies

---

## Two-Tier Configuration Management

### Tier 1: Developer/Superuser Configuration

**Visibility**: Only developers and superusers

**Configuration Items**:
- Treasury API credentials
- Inventory API credentials
- Fiscal printer credentials
- Database credentials
- Encryption keys

**Storage**:
- Encrypted at rest in database (AES-256-GCM)
- K8s secrets for runtime
- Vault for production secrets

### Tier 2: Business User Configuration

**Visibility**: Normal system users (tenant admins)

**Configuration Items**:
- Outlet settings
- Tax configuration
- Operating hours
- Pricebook settings
- Tender types

**Storage**:
- Plain text in database (non-sensitive)
- Tenant-specific configuration tables

---

## Event-Driven Architecture

### Event Catalog

#### Outbound Events (Published by POS Service)

**pos.order.created**
```json
{
  "event_id": "uuid",
  "event_type": "pos.order.created",
  "tenant_id": "tenant-uuid",
  "timestamp": "2024-12-05T10:30:00Z",
  "data": {
    "order_id": "order-uuid",
    "outlet_id": "outlet-uuid",
    "total_amount": 1500.00,
    "items": [...]
  }
}
```

**pos.order.ready**
```json
{
  "event_id": "uuid",
  "event_type": "pos.order.ready",
  "tenant_id": "tenant-uuid",
  "timestamp": "2024-12-05T10:30:00Z",
  "data": {
    "order_id": "order-uuid",
    "outlet_id": "outlet-uuid",
    "ready_at": "2024-12-05T10:35:00Z"
  }
}
```

#### Inbound Events (Consumed by POS Service)

**inventory.stock.updated**
```json
{
  "event_id": "uuid",
  "event_type": "inventory.stock.updated",
  "tenant_id": "tenant-uuid",
  "timestamp": "2024-12-05T10:30:00Z",
  "data": {
    "item_id": "item-uuid",
    "warehouse_id": "warehouse-uuid",
    "on_hand": 100,
    "available": 95
  }
}
```

---

## Integration Security

### Authentication

**JWT Tokens**:
- Validated via `shared/auth-client` library
- JWKS from auth-service
- Token claims include tenant_id for scoping

**API Keys** (Service-to-Service):
- Stored in K8s secrets
- Rotated quarterly

### Authorization

**Tenant Isolation**:
- All requests scoped by tenant_id
- Provider credentials isolated per tenant
- Data isolation enforced at database level

### Secrets Management

**Encryption**:
- Secrets encrypted at rest (AES-256-GCM)
- Decrypted only when used
- Key rotation every 90 days

### Webhook Security

**Signature Verification**:
- HMAC-SHA256 signatures
- Secret shared via K8s secret
- Timestamp validation (5-minute window)
- Nonce validation (prevent replay attacks)

---

## Error Handling & Resilience

### Retry Policies

**Exponential Backoff**:
- Initial delay: 1 second
- Max delay: 30 seconds
- Max retries: 3

### Circuit Breaker

**Implementation**:
- Opens after 5 consecutive failures
- Half-open after 60 seconds
- Closes on successful request

### Offline Resilience

**Offline Queue**:
- Redis queue for offline operations
- Automatic sync when connectivity restored
- Conflict resolution on sync

### Monitoring

**Metrics**:
- API call latency (p50, p95, p99)
- API call success/failure rates
- Event publishing success rates
- Offline queue size

**Alerts**:
- High failure rate (>5%)
- Service unavailability
- Event delivery failures
- Offline queue size threshold

---

## References

- [Auth Service Integration](../auth-service/auth-service/docs/integrations.md)
- [Inventory Service Integration](../inventory-service/inventory-api/docs/integrations.md)
- [Treasury App Integration](../finance-service/treasury-api/docs/integrations.md)
- [Logistics Service Integration](../logistics-service/logistics-api/docs/integrations.md)

