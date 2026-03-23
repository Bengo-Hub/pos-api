package events

import (
	"context"
	"database/sql"
	"fmt"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Publisher handles publishing POS events via the transactional outbox pattern.
type Publisher struct {
	repo   eventslib.OutboxRepository
	logger *zap.Logger
}

// NewPublisher creates a new POS event publisher backed by the shared-events outbox.
func NewPublisher(sqlDB *sql.DB, logger *zap.Logger) *Publisher {
	return &Publisher{
		repo:   eventslib.NewSQLOutboxRepository(sqlDB),
		logger: logger.Named("pos.events"),
	}
}

// OutboxRepo returns the outbox repository for use by the background publisher.
func (p *Publisher) OutboxRepo() eventslib.OutboxRepository {
	return p.repo
}

// publish writes an event to the outbox for background publishing to NATS.
func (p *Publisher) publish(ctx context.Context, tenantID uuid.UUID, eventType string, data map[string]any) error {
	if p == nil {
		return nil
	}

	event := eventslib.NewEvent(eventType, "pos", uuid.New(), tenantID, data)

	tx, err := p.repo.BeginTx(ctx)
	if err != nil {
		p.logger.Error("failed to begin tx for event", zap.String("event_type", eventType), zap.Error(err))
		return fmt.Errorf("begin tx: %w", err)
	}

	if err := eventslib.CreateOutboxRecordInTx(ctx, tx, p.repo, event); err != nil {
		_ = tx.Rollback()
		p.logger.Error("failed to write event to outbox", zap.String("event_type", eventType), zap.Error(err))
		return fmt.Errorf("write outbox: %w", err)
	}

	if err := tx.Commit(); err != nil {
		p.logger.Error("failed to commit event", zap.String("event_type", eventType), zap.Error(err))
		return fmt.Errorf("commit: %w", err)
	}

	p.logger.Debug("event written to outbox", zap.String("event_type", eventType))
	return nil
}

// PublishOrderCreated publishes a pos.order.created event.
func (p *Publisher) PublishOrderCreated(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "order.created", data)
}

// PublishOrderStatusChanged publishes a pos.order.status_changed event.
func (p *Publisher) PublishOrderStatusChanged(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "order.status_changed", data)
}

// PublishPaymentRecorded publishes a pos.payment.recorded event.
func (p *Publisher) PublishPaymentRecorded(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, tenantID, "payment.recorded", data)
}
