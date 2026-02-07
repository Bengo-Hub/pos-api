# POS Service - Apache Superset Integration

## Overview

The POS service integrates with the centralized Apache Superset instance for BI dashboards, analytics, and reporting. Superset is deployed as a centralized service accessible to all BengoBox services.

---

## Architecture

### Service Configuration

**Environment Variables**:
- `SUPERSET_BASE_URL` - Superset service URL
- `SUPERSET_ADMIN_USERNAME` - Admin username (K8s secret)
- `SUPERSET_ADMIN_PASSWORD` - Admin password (K8s secret)
- `SUPERSET_API_VERSION` - API version (default: v1)

**Authentication**:
- Admin credentials used for backend-to-Superset communication
- User authentication via JWT tokens passed to Superset for SSO
- Guest tokens generated for embedded dashboards

---

## Integration Methods

### 1. REST API Client

Backend uses Go HTTP client configured for Superset REST API calls.

**Base Configuration**:
- Base URL: `SUPERSET_BASE_URL/api/v1`
- Default headers: `Content-Type: application/json`
- Authentication: Bearer token from Superset login endpoint
- Retry policy: Exponential backoff (3 retries)
- Circuit breaker: Opens after 5 consecutive failures

**Key API Endpoints**:

**Authentication**:
- `POST /api/v1/security/login` - Login with admin credentials
- `POST /api/v1/security/refresh` - Refresh access token
- `POST /api/v1/security/guest_token/` - Generate guest token for embedding

**Data Sources**:
- `GET /api/v1/database/` - List all data sources
- `POST /api/v1/database/` - Create new data source
- `PUT /api/v1/database/{id}` - Update data source

**Dashboards**:
- `GET /api/v1/dashboard/` - List all dashboards
- `POST /api/v1/dashboard/` - Create new dashboard
- `GET /api/v1/dashboard/{id}` - Get dashboard details

### 2. Database Direct Connection

Superset connects directly to PostgreSQL database via read-only user for data access.

**Connection Configuration**:
- Database type: PostgreSQL
- Connection string: Provided to Superset via data source API
- Read-only user: `superset_readonly` (created in PostgreSQL)
- Permissions: SELECT only on all tables, no write access
- SSL: Required for production connections

**Read-Only User Setup**:
- Create `superset_readonly` role in PostgreSQL
- Grant CONNECT on database
- Grant USAGE on schema
- Grant SELECT on all tables
- Set default privileges for future tables

**Connection String** (for Superset):
```
postgresql://superset_readonly:password@postgresql.infra.svc.cluster.local:5432/pos_db?sslmode=require
```

---

## Pre-Built Dashboards

### 1. Sales Performance Dashboard

**Charts**:
- Sales by period (line chart)
- Sales by outlet (bar chart)
- Tender breakdown (pie chart)
- Top selling items (table)
- Average transaction value (metric)

**Filters**:
- Date range
- Outlet selection
- Tender type

**Data Source**: `pos_orders`, `pos_order_items`, `pos_outlets` tables

### 2. Cash Management Dashboard

**Charts**:
- Cash drawer status (table)
- Cash variance trends (line chart)
- Shift performance (bar chart)
- Cash transaction volume (metric)
- Variance rate (metric)

**Filters**:
- Date range
- Outlet selection
- Cashier selection

**Data Source**: `cash_drawers`, `cash_drawer_sessions`, `cash_transactions` tables

### 3. Inventory Integration Dashboard

**Charts**:
- Stock consumption trends (line chart)
- Low stock alerts (table)
- Wastage analysis (bar chart)
- Adjustment requests (table)
- Consumption rate (metric)

**Filters**:
- Date range
- Outlet selection
- Item category

**Data Source**: `stock_adjustments`, `wastage_logs` tables (linked to inventory-service)

### 4. Order Analytics Dashboard

**Charts**:
- Orders by type (pie chart)
- Order volume over time (line chart)
- Average order value (metric)
- Order completion rate (metric)
- Kitchen ticket performance (table)

**Filters**:
- Date range
- Outlet selection
- Order type

**Data Source**: `pos_orders`, `kitchen_tickets` tables

### 5. Promotions & Loyalty Dashboard

**Charts**:
- Promotion performance (bar chart)
- Gift card usage (line chart)
- Loyalty redemption rate (metric)
- Promotion ROI (metric)
- Top promotions (table)

**Filters**:
- Date range
- Outlet selection
- Promotion type

**Data Source**: `promotions`, `gift_cards`, `gift_card_transactions` tables

---

## Implementation Details

### Initialization Process

1. Authenticate with Superset using admin credentials
2. Create/update data source pointing to PostgreSQL
3. Create/update dashboards for each module:
   - Sales Performance Dashboard
   - Cash Management Dashboard
   - Inventory Integration Dashboard
   - Order Analytics Dashboard
   - Promotions & Loyalty Dashboard
4. Log warnings for dashboard creation failures (non-blocking)

### Dashboard Bootstrap

**Backend Endpoint**: `GET /api/v1/dashboards/{module}/embed`

**Process**:
1. Extract tenant ID from context
2. Get dashboard ID for module from Superset
3. Generate guest token with Row-Level Security (RLS) clause filtering by tenant_id
4. Construct embed URL with dashboard ID and guest token
5. Return embed URL with expiration time (5 minutes)

### Row-Level Security (RLS)

**Implementation**:
- Guest tokens include RLS clauses
- RLS filters data by `tenant_id`
- Each tenant sees only their data

---

## Error Handling

### Retry Logic

**Retry Policy**:
- Maximum 3 retry attempts
- Exponential backoff (1s, 2s, 4s delays)
- Retry on 5xx errors or network failures
- Return response on success or after max retries

### Circuit Breaker

**Implementation**:
- Opens after 5 consecutive failures
- Half-open after 60 seconds
- Closes on successful request

### Fallback Strategies

**Superset Unavailable**:
- Return cached dashboard URLs (if available)
- Show static dashboard images
- Log error for monitoring
- Alert operations team

---

## Monitoring

### Metrics

**Integration-Specific Metrics**:
- Superset API call latency (p50, p95, p99)
- Dashboard creation/update success rates
- Guest token generation latency
- Data source connection health

**Prometheus Metrics**:
- `superset_api_call_duration_seconds` - Histogram of API call durations (labeled by endpoint, status)
- `superset_dashboard_views_total` - Counter of dashboard views (labeled by dashboard, tenant)

### Alerts

**Alert Conditions**:
- Superset service unavailability
- High API call failure rate (>5%)
- Dashboard creation failures
- Data source connection failures

---

## Security Considerations

### Authentication & Authorization

- Admin credentials stored in K8s secrets
- Guest tokens expire after 5 minutes
- RLS ensures tenant data isolation
- JWT tokens validated for SSO

### Data Privacy

- Read-only database user
- RLS filters enforce tenant isolation
- Sensitive data masked in logs
- PII data excluded from dashboards (if applicable)

---

## References

- [Apache Superset REST API Documentation](https://superset.apache.org/docs/api)
- [Superset Deployment Guide](../../devops-k8s/docs/superset-deployment.md)
- [Ordering Service Superset Integration](../../../ordering-service/ordering-backend/docs/superset-integration.md)

