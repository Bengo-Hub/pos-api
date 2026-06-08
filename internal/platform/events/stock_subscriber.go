package events

import (
	"context"
	"fmt"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
)

// StockSubscriber listens to inventory stock events and:
//   - inventory.stock.low  → re-publishes as pos.alert.stock_low for notifications-service
//   - inventory.stock.out  → marks POSCatalogOverride.is_available = false (recipe ingredient depleted)
//   - inventory.stock.in   → marks POSCatalogOverride.is_available = true  (ingredients restocked)
type StockSubscriber struct {
	publisher *Publisher
	client    *ent.Client
	log       *zap.Logger
}

func NewStockSubscriber(publisher *Publisher, client *ent.Client, log *zap.Logger) *StockSubscriber {
	return &StockSubscriber{
		publisher: publisher,
		client:    client,
		log:       log.Named("pos.stock_subscriber"),
	}
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

	// inventory.stock.low → alert
	SubscribeWithRebind(s.log, js, "inventory.stock.low", func(msg *nats.Msg) {
		evt, parseErr := sharedevents.FromJSON(msg.Data)
		if parseErr != nil {
			s.log.Error("stock.low: unmarshal failed", zap.Error(parseErr))
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
	},
		nats.Durable("pos-inv-stock-low"),
		nats.AckExplicit(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)

	// inventory.stock.out → mark POSCatalogOverride unavailable (recipe ingredient depleted)
	SubscribeWithRebind(s.log, js, "inventory.stock.out", func(msg *nats.Msg) {
		evt, parseErr := sharedevents.FromJSON(msg.Data)
		if parseErr != nil {
			s.log.Error("stock.out: unmarshal failed", zap.Error(parseErr))
			_ = msg.Nak()
			return
		}
		if err := s.handleStockOut(context.Background(), evt); err != nil {
			s.log.Error("stock.out: handler failed", zap.Error(err))
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	},
		nats.Durable("pos-inv-stock-out"),
		nats.DeliverAll(),
		nats.AckExplicit(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(5),
	)

	// inventory.stock.in → re-enable POSCatalogOverride when ingredients are restocked
	SubscribeWithRebind(s.log, js, "inventory.stock.in", func(msg *nats.Msg) {
		evt, parseErr := sharedevents.FromJSON(msg.Data)
		if parseErr != nil {
			s.log.Error("stock.in: unmarshal failed", zap.Error(parseErr))
			_ = msg.Nak()
			return
		}
		if err := s.handleStockIn(context.Background(), evt); err != nil {
			s.log.Error("stock.in: handler failed", zap.Error(err))
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	},
		nats.Durable("pos-inv-stock-in"),
		nats.DeliverAll(),
		nats.AckExplicit(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(5),
	)

	s.log.Info("pos stock event subscriptions active",
		zap.Strings("subjects", []string{
			"inventory.stock.low",
			"inventory.stock.out",
			"inventory.stock.in",
		}))
	return nil
}

// setSkuAvailability toggles POS catalog availability for a SKU. When the event
// carries an outlet_id it UPSERTS the (tenant, outlet, sku) override so a
// default-available item with no override row is still toggled — the old
// UPDATE-only path affected 0 rows for such items, leaving sold-out recipes
// sellable at the till. selling_price stays nil, so POS keeps falling back to
// inventory pricing. Falls back to a sku-wide update when no outlet_id is present.
func (s *StockSubscriber) setSkuAvailability(ctx context.Context, tenantID uuid.UUID, outletRaw, sku string, available bool) (int, error) {
	if outletID, perr := uuid.Parse(outletRaw); outletRaw != "" && perr == nil {
		// The (tenant, outlet, sku) index on pos_catalog_overrides is NOT unique
		// (outlet_id is nullable), so ON CONFLICT can't be used (42P10). Do an
		// explicit find-or-create/update instead. selling_price stays nil → POS
		// keeps falling back to inventory pricing.
		existing, qerr := s.client.POSCatalogOverride.Query().
			Where(
				entoverride.TenantID(tenantID),
				entoverride.OutletID(outletID),
				entoverride.InventorySku(sku),
			).
			First(ctx)
		if qerr == nil && existing != nil {
			if _, uerr := existing.Update().
				SetIsAvailable(available).
				SetUpdatedAt(time.Now()).
				Save(ctx); uerr != nil {
				return 0, uerr
			}
			return 1, nil
		}
		if qerr != nil && !ent.IsNotFound(qerr) {
			return 0, qerr
		}
		if _, cerr := s.client.POSCatalogOverride.Create().
			SetTenantID(tenantID).
			SetOutletID(outletID).
			SetInventorySku(sku).
			SetIsAvailable(available).
			Save(ctx); cerr != nil {
			return 0, cerr
		}
		return 1, nil
	}
	return s.client.POSCatalogOverride.Update().
		Where(
			entoverride.TenantID(tenantID),
			entoverride.InventorySku(sku),
		).
		SetIsAvailable(available).
		Save(ctx)
}

// handleStockOut marks POSCatalogOverride.is_available = false for the depleted SKU.
func (s *StockSubscriber) handleStockOut(ctx context.Context, evt *sharedevents.Event) error {
	if s.client == nil {
		return nil
	}
	tenantID := evt.TenantID
	if tenantID == uuid.Nil {
		return fmt.Errorf("stock.out: missing tenant_id")
	}
	sku, _ := evt.Payload["sku"].(string)
	if sku == "" {
		return fmt.Errorf("stock.out: missing sku")
	}
	outletRaw, _ := evt.Payload["outlet_id"].(string)
	count, err := s.setSkuAvailability(ctx, tenantID, outletRaw, sku, false)
	if err != nil {
		return fmt.Errorf("stock.out: update override: %w", err)
	}
	s.log.Info("pos catalog: item marked unavailable (stock-out)",
		zap.String("sku", sku),
		zap.String("tenant_id", tenantID.String()),
		zap.Int("overrides_updated", count))
	return nil
}

// handleStockIn re-enables POSCatalogOverride.is_available when all ingredients are restocked.
func (s *StockSubscriber) handleStockIn(ctx context.Context, evt *sharedevents.Event) error {
	if s.client == nil {
		return nil
	}
	tenantID := evt.TenantID
	if tenantID == uuid.Nil {
		return fmt.Errorf("stock.in: missing tenant_id")
	}
	sku, _ := evt.Payload["sku"].(string)
	if sku == "" {
		return fmt.Errorf("stock.in: missing sku")
	}
	outletRaw, _ := evt.Payload["outlet_id"].(string)
	count, err := s.setSkuAvailability(ctx, tenantID, outletRaw, sku, true)
	if err != nil {
		return fmt.Errorf("stock.in: update override: %w", err)
	}
	s.log.Info("pos catalog: item re-enabled (ingredients restocked)",
		zap.String("sku", sku),
		zap.String("tenant_id", tenantID.String()),
		zap.Int("overrides_updated", count))
	return nil
}
