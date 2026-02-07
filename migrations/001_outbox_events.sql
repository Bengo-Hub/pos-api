-- Outbox Events table for transactional outbox pattern
-- This table stores events atomically with domain operations for reliable publishing

CREATE TABLE IF NOT EXISTS outbox_events (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL,
    aggregate_type VARCHAR(100) NOT NULL,
    aggregate_id UUID NOT NULL,
    event_type VARCHAR(200) NOT NULL,
    payload BYTEA NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'PENDING',
    attempts INTEGER NOT NULL DEFAULT 0,
    last_attempt_at TIMESTAMPTZ,
    published_at TIMESTAMPTZ,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT outbox_events_status_check CHECK (status IN ('PENDING', 'PUBLISHED', 'FAILED'))
);

-- Index for fetching pending events (primary query for publisher)
CREATE INDEX IF NOT EXISTS idx_outbox_events_status_created
    ON outbox_events (status, created_at)
    WHERE status = 'PENDING';

-- Index for querying events by aggregate (debugging/replay)
CREATE INDEX IF NOT EXISTS idx_outbox_events_aggregate
    ON outbox_events (aggregate_type, aggregate_id);

-- Index for tenant-scoped queries
CREATE INDEX IF NOT EXISTS idx_outbox_events_tenant_status
    ON outbox_events (tenant_id, status);

COMMENT ON TABLE outbox_events IS 'Transactional outbox for reliable event publishing';
COMMENT ON COLUMN outbox_events.aggregate_type IS 'Entity type: pos_order, cash_drawer, pricebook, etc.';
COMMENT ON COLUMN outbox_events.event_type IS 'Event name: pos.order.created, pos.drawer.opened, etc.';
