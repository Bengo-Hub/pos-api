package events

import (
	"context"
	"fmt"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// StockSubscriber listens to inventory.stock.low events and re-publishes
// them as pos.alert.stock_low so notifications-service can alert managers.
type StockSubscriber struct {
	publisher *Publisher
	log       *zap.Logger
}

func NewStockSubscriber(publisher *Publisher, log *zap.Logger) *StockSubscriber {
	return &StockSubscriber{publisher: publisher, log: log.Named("pos.stock_subscriber")}
}

func (s *StockSubscriber) Subscribe(nc *nats.Conn) error {
	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("pos stock subscriber: jetstream init: %w", err)
	}

	// Ensure inventory stream exists (inventory-api owns it; pos-api just consumes).
	if _, err := js.StreamInfo("inventory"); err != nil {
		if _, addErr := js.AddStream(&nats.StreamConfig{
			Name:      "inventory",
			Subjects:  []string{"inventory.>"},
			Retention: nats.LimitsPolicy,
			MaxAge:    72 * time.Hour,
			Storage:   nats.FileStorage,
		}); addErr != nil && addErr != nats.ErrStreamNameAlreadyInUse {
			return fmt.Errorf("pos stock subscriber: ensure inventory stream: %w", addErr)
		}
	}

	handler := func(msg *nats.Msg) {
		evt, err := sharedevents.FromJSON(msg.Data)
		if err != nil {
			s.log.Error("stock.low: unmarshal failed", zap.Error(err))
			_ = msg.Nak()
			return
		}

		payload := evt.Payload
		tenantIDStr, _ := payload["tenant_id"].(string)
		tenantID, parseErr := uuid.Parse(tenantIDStr)
		if parseErr != nil {
			s.log.Warn("stock.low: invalid tenant_id, skipping", zap.String("tenant_id", tenantIDStr))
			_ = msg.Ack()
			return
		}

		alertPayload := map[string]any{
			"sku":           payload["sku"],
			"item_name":     payload["item_name"],
			"current_qty":   payload["current_qty"],
			"reorder_point": payload["reorder_point"],
			"outlet_id":     payload["outlet_id"],
			"outlet_name":   payload["outlet_name"],
			"tenant_id":     tenantIDStr,
		}
		if err := s.publisher.PublishStockAlert(context.Background(), tenantID, alertPayload); err != nil {
			s.log.Error("stock.low: failed to publish pos.alert.stock_low", zap.Error(err))
			_ = msg.Nak()
			return
		}

		s.log.Info("stock.low forwarded as pos.alert.stock_low",
			zap.String("sku", fmt.Sprintf("%v", payload["sku"])),
			zap.String("tenant_id", tenantIDStr),
		)
		_ = msg.Ack()
	}

	_, err = js.Subscribe("inventory.stock.low", handler,
		nats.Durable("pos-inv-stock-low"),
		nats.AckExplicit(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
	return err
}
