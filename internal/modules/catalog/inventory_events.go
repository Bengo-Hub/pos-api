package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	sharedevents "github.com/Bengo-Hub/shared-events"

	"github.com/bengobox/pos-service/internal/ent"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
)

// InventoryEventHandler syncs POS-specific compliance flags from inventory events.
// Item data (name, description, image) is always fetched fresh from inventory-api at request time.
// Only flags that POS needs to enforce locally (pharmacy, age-gate, etc.) are stored.
type InventoryEventHandler struct {
	client *ent.Client
	logger *zap.Logger
}

// NewInventoryEventHandler creates a new inventory event handler.
func NewInventoryEventHandler(client *ent.Client, logger *zap.Logger) *InventoryEventHandler {
	return &InventoryEventHandler{
		client: client,
		logger: logger.Named("pos.catalog.inventory_events"),
	}
}

// SubscribeToInventoryEvents subscribes to inventory.item.* JetStream subjects.
func (h *InventoryEventHandler) SubscribeToInventoryEvents(nc *nats.Conn) error {
	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("pos catalog: jetstream init: %w", err)
	}

	if _, err := js.StreamInfo("inventory"); err != nil {
		_, addErr := js.AddStream(&nats.StreamConfig{
			Name:      "inventory",
			Subjects:  []string{"inventory.>"},
			Retention: nats.LimitsPolicy,
			MaxAge:    72 * time.Hour,
			Storage:   nats.FileStorage,
		})
		if addErr != nil && addErr != nats.ErrStreamNameAlreadyInUse {
			return fmt.Errorf("pos catalog: ensure inventory stream: %w", addErr)
		}
	}

	handler := func(msg *nats.Msg) {
		evt, err := sharedevents.FromJSON(msg.Data)
		if err != nil {
			h.logger.Error("catalog sync: unmarshal event failed", zap.Error(err))
			_ = msg.Nak()
			return
		}
		if err := h.syncComplianceFlags(context.Background(), evt); err != nil {
			h.logger.Error("catalog sync: compliance flag sync failed", zap.Error(err))
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	}

	type sub struct {
		subject string
		durable string
	}
	subs := []sub{
		{"inventory.item.created", "pos-inv-item-created"},
		{"inventory.item.updated", "pos-inv-item-updated"},
	}
	for _, s := range subs {
		if _, err := js.Subscribe(s.subject, handler,
			nats.Durable(s.durable),
			nats.AckExplicit(),
			nats.AckWait(30*time.Second),
			nats.MaxDeliver(5),
			nats.DeliverAll(),
		); err != nil {
			h.logger.Warn("pos catalog: subscribe failed",
				zap.String("subject", s.subject), zap.Error(err))
		}
	}

	h.logger.Info("inventory catalog sync subscriptions active",
		zap.String("subjects", "inventory.item.created, inventory.item.updated"))
	return nil
}

// syncComplianceFlags upserts POS-specific compliance fields on POSCatalogOverride
// when an inventory item is created or updated. Does NOT store item name/description/image
// since those are always fetched fresh from inventory-api at request time.
func (h *InventoryEventHandler) syncComplianceFlags(ctx context.Context, evt *sharedevents.Event) error {
	sku, _ := evt.Payload["sku"].(string)
	itemType, _ := evt.Payload["type"].(string)
	if sku == "" {
		return nil
	}

	// Skip non-sellable inventory types — only GOODS, RECIPE, SERVICE, VOUCHER belong in POS
	if itemType == "INGREDIENT" || itemType == "EQUIPMENT" {
		return nil
	}

	requiresAgeVerification, _ := evt.Payload["requires_age_verification"].(bool)
	isControlledSubstance, _ := evt.Payload["is_controlled_substance"].(bool)
	isActive, _ := evt.Payload["is_active"].(bool)

	if evt.TenantID.String() == "00000000-0000-0000-0000-000000000000" {
		return nil
	}
	tenantID := evt.TenantID

	// Only upsert compliance flags — don't create override rows for items that have no POS-specific config yet
	existing, _ := h.client.POSCatalogOverride.Query().
		Where(entoverride.TenantID(tenantID), entoverride.InventorySku(sku)).
		First(ctx)

	if existing != nil {
		// Patch compliance flags from inventory event
		upd := existing.Update().
			SetRequiresAgeVerification(requiresAgeVerification).
			SetIsControlledSubstance(isControlledSubstance).
			SetIsAvailable(isActive)
		if _, err := upd.Save(ctx); err != nil {
			return fmt.Errorf("update compliance flags for %s: %w", sku, err)
		}
		h.logger.Debug("POS catalog compliance flags updated",
			zap.String("sku", sku),
			zap.Bool("controlled", isControlledSubstance),
			zap.Bool("age_gate", requiresAgeVerification))
	}
	// If no override exists yet, skip — it will be created when a price is set via admin

	return nil
}
