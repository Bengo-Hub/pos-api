package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// Publisher handles publishing POS events to NATS JetStream.
type Publisher struct {
	js     nats.JetStreamContext
	logger *zap.Logger
}

// NewPublisher creates a new POS event publisher.
func NewPublisher(nc *nats.Conn, logger *zap.Logger) (*Publisher, error) {
	if nc == nil {
		return nil, fmt.Errorf("nats connection is nil")
	}
	js, err := nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("jetstream init: %w", err)
	}
	return &Publisher{
		js:     js,
		logger: logger.Named("pos.events"),
	}, nil
}

// posEvent is the CloudEvents-compatible envelope for POS events.
type posEvent struct {
	ID              string         `json:"id"`
	Source          string         `json:"source"`
	SpecVersion    string         `json:"specversion"`
	Type           string         `json:"type"`
	DataContentType string        `json:"datacontenttype"`
	Time           string         `json:"time"`
	TenantID       string         `json:"tenantId"`
	Data           map[string]any `json:"data"`
}

// publish sends an event to the specified subject via JetStream.
func (p *Publisher) publish(ctx context.Context, subject string, eventType string, tenantID uuid.UUID, data map[string]any) error {
	if p == nil {
		return nil
	}

	evt := posEvent{
		ID:              uuid.New().String(),
		Source:          "pos-service",
		SpecVersion:    "1.0",
		Type:           eventType,
		DataContentType: "application/json",
		Time:           time.Now().UTC().Format(time.RFC3339),
		TenantID:       tenantID.String(),
		Data:           data,
	}

	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	if _, err := p.js.Publish(subject, payload); err != nil {
		p.logger.Error("failed to publish event",
			zap.Error(err),
			zap.String("subject", subject),
			zap.String("type", eventType))
		return fmt.Errorf("publish event: %w", err)
	}

	p.logger.Debug("event published",
		zap.String("subject", subject),
		zap.String("type", eventType))

	return nil
}

// PublishOrderCreated publishes a pos.order.created event.
func (p *Publisher) PublishOrderCreated(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, "pos.order.created", "pos.order.created", tenantID, data)
}

// PublishOrderStatusChanged publishes a pos.order.status_changed event.
func (p *Publisher) PublishOrderStatusChanged(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, "pos.order.status_changed", "pos.order.status_changed", tenantID, data)
}

// PublishPaymentRecorded publishes a pos.payment.recorded event.
func (p *Publisher) PublishPaymentRecorded(ctx context.Context, tenantID uuid.UUID, data map[string]any) error {
	return p.publish(ctx, "pos.payment.recorded", "pos.payment.recorded", tenantID, data)
}
