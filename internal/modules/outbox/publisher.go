package outbox

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/Bengo-Hub/shared-events"
	"go.uber.org/zap"
)

// EventPublisher is the interface for publishing events to a message broker.
type EventPublisher interface {
	Publish(ctx context.Context, event *events.Event) error
}

// Publisher polls the outbox and publishes events to a message broker.
type Publisher struct {
	repo       *PgxRepository
	publisher  EventPublisher
	logger     *zap.Logger
	batchSize  int
	pollPeriod time.Duration

	wg     sync.WaitGroup
	stopCh chan struct{}
}

// PublisherConfig holds publisher configuration.
type PublisherConfig struct {
	BatchSize  int
	PollPeriod time.Duration
}

// DefaultPublisherConfig returns sensible defaults.
func DefaultPublisherConfig() PublisherConfig {
	return PublisherConfig{
		BatchSize:  100,
		PollPeriod: 5 * time.Second,
	}
}

// NewPublisher creates a new outbox publisher.
func NewPublisher(repo *PgxRepository, publisher EventPublisher, logger *zap.Logger, cfg PublisherConfig) *Publisher {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.PollPeriod <= 0 {
		cfg.PollPeriod = 5 * time.Second
	}
	return &Publisher{
		repo:       repo,
		publisher:  publisher,
		logger:     logger.Named("outbox.publisher"),
		batchSize:  cfg.BatchSize,
		pollPeriod: cfg.PollPeriod,
		stopCh:     make(chan struct{}),
	}
}

// Start begins the background polling loop.
func (p *Publisher) Start(ctx context.Context) {
	p.wg.Add(1)
	go p.run(ctx)
	p.logger.Info("outbox publisher started",
		zap.Int("batch_size", p.batchSize),
		zap.Duration("poll_period", p.pollPeriod),
	)
}

// Stop gracefully stops the publisher.
func (p *Publisher) Stop() {
	close(p.stopCh)
	p.wg.Wait()
	p.logger.Info("outbox publisher stopped")
}

func (p *Publisher) run(ctx context.Context) {
	defer p.wg.Done()

	ticker := time.NewTicker(p.pollPeriod)
	defer ticker.Stop()

	// Do initial poll immediately
	p.poll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

func (p *Publisher) poll(ctx context.Context) {
	records, err := p.repo.GetPendingRecords(ctx, p.batchSize)
	if err != nil {
		p.logger.Error("failed to get pending records", zap.Error(err))
		return
	}

	if len(records) == 0 {
		return
	}

	p.logger.Debug("processing outbox records", zap.Int("count", len(records)))

	for _, record := range records {
		if err := p.publishRecord(ctx, record); err != nil {
			p.logger.Warn("failed to publish record",
				zap.String("id", record.ID.String()),
				zap.String("event_type", record.EventType),
				zap.Error(err),
			)
			// Mark as failed (with retry logic in MarkAsFailed)
			if markErr := p.repo.MarkAsFailed(ctx, record.ID, err.Error(), time.Now()); markErr != nil {
				p.logger.Error("failed to mark record as failed",
					zap.String("id", record.ID.String()),
					zap.Error(markErr),
				)
			}
			continue
		}

		// Mark as published
		if err := p.repo.MarkAsPublished(ctx, record.ID, time.Now()); err != nil {
			p.logger.Error("failed to mark record as published",
				zap.String("id", record.ID.String()),
				zap.Error(err),
			)
		}
	}
}

func (p *Publisher) publishRecord(ctx context.Context, record *events.OutboxRecord) error {
	// The payload stored is the full JSON event from ToJSON()
	// Unmarshal it back into an Event struct
	var event events.Event
	if err := json.Unmarshal(record.Payload, &event); err != nil {
		// If unmarshal fails, construct event from record fields
		event = events.Event{
			ID:            record.ID,
			TenantID:      record.TenantID,
			AggregateType: record.AggregateType,
			AggregateID:   record.AggregateID,
			EventType:     record.EventType,
			Timestamp:     record.CreatedAt,
		}
	}

	// Publish to message broker
	return p.publisher.Publish(ctx, &event)
}
