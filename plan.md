# POS Service - Implementation Plan

## Executive Summary

**System Purpose**: Multi-tenant, cloud-ready Point-of-Sale backend that powers third-party outlets (cafés/bars, quick-service restaurants, supermarkets, ecommerce fulfilment, kitchen displays, kiosks). Provides composable API layer for embedded client surfaces.

**Key Capabilities**:
- Multi-tenant POS with outlet management
- Order capture and ticketing
- Tendering and cash management
- Catalog and pricebook management
- Kitchen and fulfillment coordination
- Promotions, loyalty, and gift cards
- Reporting and analytics

**Entity Ownership**: This service owns POS-specific entities: POS orders, devices, sessions, cash drawers, POS payments (references), promotions, gift cards, price books, table management, and bar tabs. **POS does NOT own**: catalog items (references inventory-service), users (references auth-service via `user_id`), payment processing (uses treasury-api), inventory balances (queries inventory-service).

---

## Technology Stack

### Core Framework
- **Language**: Go 1.22+
- **Architecture**: Clean/Hexagonal architecture
- **HTTP Router**: chi
- **API Documentation**: OpenAPI-first contracts
- **gRPC**: ConnectRPC (optional) for gRPC wrappers

### Data & Caching
- **Primary Database**: PostgreSQL 16+
- **ORM**: Ent (schema-as-code migrations)
- **Caching**: Redis 7+ for sessions, offline queues, rate limiting

### Supporting Libraries
- **Validation**: go-playground/validator
- **Configuration**: kelseyhightower/envconfig
- **Logging**: zap (structured logging)
- **Tracing**: OpenTelemetry instrumentation
- **Metrics**: Prometheus

### DevOps & Observability
- **Containerization**: Multi-stage Docker builds
- **Orchestration**: Kubernetes (via centralized devops-k8s)
- **CI/CD**: GitHub Actions → ArgoCD
- **Monitoring**: Prometheus + Grafana, OpenTelemetry
- **APM**: Jaeger distributed tracing

---

## Domain Modules & Features

### 1. Tenant & Outlet Management

**POS-Specific Features**:
- Tenant registration and outlet mapping
- Device provisioning (POS terminals/tablets/kiosks)
- Configuration profiles (timezone, tax, operating hours)

**Entities Owned**:
- `pos_outlets` - POS outlet definitions
- `pos_devices` - Device registry
- `outlet_configurations` - Outlet-specific settings

**Integration Points**:
- **auth-service**: Outlet registry (references only)
- **Tenant Sync**: Webhook events for tenant/outlet discovery

### 2. User, Roles & Sessions

**POS-Specific Features**:
- POS-specific RBAC roles (cashier, supervisor, manager)
- Device sign-in (PIN, RFID, OAuth tokens)
- Session heartbeat tracking
- Device-level permissions

**Entities Owned**:
- `pos_sessions` - Active POS sessions
- `pos_roles` - POS-specific roles
- `pos_permissions` - POS-specific permissions

**Integration Points**:
- **auth-service**: User identity sync (references only)

### 3. Catalog & Pricebook

**POS-Specific Features**:
- Catalog sync from inventory-service (read-only cache)
- Multiple pricebooks per outlet
- Local overrides, modifiers, bundles
- Barcode/PLU mapping

**Entities Owned**:
- `pricebooks` - Pricebook definitions
- `pricebook_items` - Pricebook item mappings
- `catalog_cache` - Cached catalog items (read-only)

**Integration Points**:
- **inventory-service**: Catalog sync (read-only cache, no duplication)

### 4. Order Capture & Ticketing

**POS-Specific Features**:
- POS order creation (dine-in, take-out, delivery, pickup)
- Split bills, table assignments
- Service charges, tip prompts
- Real-time order events

**Entities Owned**:
- `pos_orders` - POS order records
- `pos_order_items` - Order line items
- `pos_order_events` - Order lifecycle events
- `tables` - Table management
- `bar_tabs` - Bar tab management

**Integration Points**:
- **inventory-service**: Stock consumption events
- **treasury-api**: Payment processing
- **logistics-service**: Pickup task creation

### 5. Tendering & Cash Management

**POS-Specific Features**:
- Multiple tender types (cash, card, mobile money, gift cards)
- Cash drawer lifecycle
- Float setup, shift open/close
- Cash audits and discrepancies

**Entities Owned**:
- `cash_drawers` - Cash drawer definitions
- `cash_drawer_sessions` - Drawer session tracking
- `cash_transactions` - Cash transaction records
- `tender_types` - Tender type definitions

**Integration Points**:
- **treasury-api**: Payment processing, settlement reconciliation

### 6. Inventory & Stock Adjustments

**POS-Specific Features**:
- Inventory deduction at sale
- Wastage tracking
- Returns processing
- Stock adjustment requests

**Entities Owned**:
- `stock_adjustments` - Adjustment requests
- `wastage_logs` - Wastage tracking

**Integration Points**:
- **inventory-service**: Stock consumption, adjustment requests

### 7. Kitchen & Fulfilment Coordination

**POS-Specific Features**:
- Kitchen production queue routing
- Order ready notifications
- Expedite signals
- Re-fire requests

**Entities Owned**:
- `kitchen_tickets` - Kitchen ticket records
- `kitchen_ticket_events` - Ticket lifecycle events

**Integration Points**:
- **logistics-service**: Pickup task creation
- **notifications-service**: Order ready notifications

### 8. Promotions, Loyalty & Gift Cards

**POS-Specific Features**:
- Promo code application
- Auto-discounts
- Gift card issuance and redemption
- Loyalty integration

**Entities Owned**:
- `promotions` - Promotion definitions
- `gift_cards` - Gift card records
- `gift_card_transactions` - Gift card transactions

**Integration Points**:
- **cafe-backend**: Loyalty integration
- **treasury-api**: Gift card payment processing

### 9. Reporting & Analytics

**POS-Specific Features**:
- Sales reports (daily/weekly/monthly)
- Tender breakdown
- Tax reports
- Cashier performance

**Entities Owned**:
- `report_jobs` - Report generation jobs

**Integration Points**:
- **Apache Superset**: BI dashboards and analytics

### 10. Audit, Compliance & Security

**POS-Specific Features**:
- Audit trails for voids, discounts, adjustments
- User login tracking
- Configurable retention policies
- GDPR/Kenyan DPA compliance

**Entities Owned**:
- `audit_logs` - Audit trail records
- `compliance_reports` - Compliance reporting

---

## Cross-Cutting Concerns

### Testing
- Go test suites with table-driven tests
- Testcontainers for integration testing
- Pact for contract tests
- Performance testing

### Observability
- Structured logging (zap)
- Tracing via OpenTelemetry
- Metrics exported via Prometheus
- Distributed tracing via Tempo/Jaeger

### Security
- OWASP ASVS baseline
- TLS everywhere
- Secrets via Vault/Parameter Store
- Rate limiting & anomaly detection middleware
- JWT validation via auth-service

### Scalability
- Stateless HTTP layer
- Background workers via NATS/Redis streams
- Offline queue support
- Smart caching strategy

### Data Modelling
- Ent schemas as single source of truth
- Tenant/outlet discovery webhooks
- Outbox pattern for reliable domain events (using `shared-events` library)
- Offline queue for resilience

### Architecture Patterns Migration Status (January 2026)

| Pattern | Status | Library | Notes |
|---------|--------|---------|-------|
| Outbox Pattern | ✅ **Schema Ready** | `shared-events` v0.1.0 | Migration + repository created |
| Circuit Breaker | ⏳ **Dependency Ready** | `shared-service-client` v0.1.0 | Import and use in HTTP clients |
| Shared Middleware | ✅ **Completed** | `httpware` v0.1.1 | Migrated to shared package |
| JWT Validation | ✅ Implemented | `shared-auth-client` v0.2.0 | Production - supports JWT + API Key |
| Subscription Feature Gating | ⏳ **Pending** | `shared-auth-client` v0.2.0 | Upgrade required for feature gating |

**Migration Checklist:**
- [x] Add `github.com/Bengo-Hub/shared-events` dependency ✅ (Jan 2026)
- [x] Create `outbox_events` SQL migration ✅ (Jan 2026)
- [x] Create `internal/modules/outbox/repository.go` ✅ (Jan 2026)
- [ ] Replace direct NATS publish with `PublishWithOutbox`
- [ ] Add background publisher worker
- [ ] Add `github.com/Bengo-Hub/shared-service-client` dependency
- [ ] Replace direct HTTP calls with shared client
- [x] Add `github.com/Bengo-Hub/httpware` dependency ✅ (Jan 2026)
- [x] Replace local middleware with shared package ✅ (Jan 2026)
- [x] Upgrade `shared-auth-client` to v0.2.0 ✅ (Jan 2026)
- [ ] Implement dual authentication (JWT Bearer + API Key)
- [ ] Add subscription feature gates to premium POS features

### Subscription Feature Gating

POS Service uses `shared-auth-client` v0.2.0 for subscription-based feature gating. Premium POS features are gated based on tenant subscription plan:

**Features to Gate:**

| Feature Code | Description | Minimum Plan |
|--------------|-------------|--------------|
| `multi_outlet` | Multi-outlet management | GROWTH |
| `kitchen_display` | Kitchen display system | GROWTH |
| `table_management` | Table and reservation management | PROFESSIONAL |
| `bar_tabs` | Bar tab management | PROFESSIONAL |
| `advanced_promotions` | Complex promotion rules engine | PROFESSIONAL |
| `loyalty_integration` | Loyalty program integration | PROFESSIONAL |
| `gift_cards` | Gift card issuance and redemption | GROWTH |
| `offline_mode` | Offline POS operations | PROFESSIONAL |
| `custom_reports` | Custom report builder | ENTERPRISE |
| `multi_pricebook` | Multiple pricebooks per outlet | GROWTH |
| `kds_routing` | Advanced KDS routing rules | ENTERPRISE |

**Implementation Example:**

```go
import authclient "github.com/Bengo-Hub/shared-auth-client"

// Router setup with feature gating
r.Route("/v1/{tenant}/kitchen-display", func(r chi.Router) {
    r.Use(authMiddleware.RequireAuth)
    r.Use(authclient.RequireFeature("kitchen_display"))
    r.Get("/tickets", h.ListKitchenTickets)
    r.Post("/tickets/{id}/complete", h.CompleteTicket)
})

r.Route("/v1/{tenant}/bar-tabs", func(r chi.Router) {
    r.Use(authMiddleware.RequireAuth)
    r.Use(authclient.RequireFeature("bar_tabs"))
    r.Get("/", h.ListBarTabs)
    r.Post("/", h.CreateBarTab)
})

// Plan-based gating for enterprise features
r.Route("/v1/{tenant}/reports/custom", func(r chi.Router) {
    r.Use(authMiddleware.RequireAuth)
    r.Use(authclient.RequirePlan("ENTERPRISE"))
    r.Get("/", h.ListCustomReports)
    r.Post("/", h.CreateCustomReport)
})
```

**Dual Authentication Support:**

```go
// Initialize with both JWT and API Key validators
validator := authclient.NewValidator(authclient.ValidatorConfig{
    JWKSURL:  cfg.JWKSURL,
    Issuer:   cfg.JWTIssuer,
    Audience: cfg.JWTAudience,
})

apiKeyValidator := authclient.NewAPIKeyValidator(authclient.APIKeyConfig{
    AuthServiceURL: cfg.AuthServiceURL,
    CacheTTL:       5 * time.Minute,
})

authMiddleware := authclient.NewAuthMiddlewareWithAPIKey(validator, apiKeyValidator)
```

---

## API & Protocol Strategy

- **REST-first**: Versioned routes (`/v1/{tenant}/orders`), documented via OpenAPI
- **gRPC**: ConnectRPC for high-throughput operations
- **Webhooks**: Order events, payment callbacks
- **SSE**: Real-time order updates
- **Idempotency**: Keys, correlation IDs, distributed tracing context propagation

---

## Compliance & Risk Controls

- Align with Kenya Data Protection Act: explicit consent flows, user data export/delete endpoints, audit logging
- Financial compliance: cash handling, tax reporting
- Disaster recovery playbook, RTO/RPO targets (<1 hour)

---

## Sprint Delivery Plan

See `docs/sprints/` folder for detailed sprint plans:
- Sprint 0: Foundations
- Sprint 1: Tenant & Device Management
- Sprint 2: Catalog & Pricebook
- Sprint 3: Order Capture & Ticketing
- Sprint 4: Tendering & Cash Management
- Sprint 5: Inventory Integration
- Sprint 6: Kitchen & Fulfilment
- Sprint 7: Promotions & Loyalty
- Sprint 8: Reporting & Analytics
- Sprint 9: Compliance & Hardening
- Sprint 10: Launch & Handover

---

## Runtime Ports & Environments

- **Local development**: Service runs on port **4105**
- **Cloud deployment**: All backend services listen on **port 4000** for consistency behind ingress controllers

---

## References

- [Integration Guide](docs/integrations.md)
- [Entity Relationship Diagram](docs/erd.md)
- [Superset Integration](docs/superset-integration.md)
- [Sprint Plans](docs/sprints/)
