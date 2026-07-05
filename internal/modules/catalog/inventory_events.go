package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	sharedevents "github.com/Bengo-Hub/shared-events"

	"github.com/bengobox/pos-service/internal/ent"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
)

// uuidFromPayload parses a UUID from an event payload value (string after JSON round-trip).
func uuidFromPayload(v any) *uuid.UUID {
	switch t := v.(type) {
	case string:
		if id, err := uuid.Parse(t); err == nil {
			return &id
		}
	case uuid.UUID:
		return &t
	}
	return nil
}

// intPtrFromPayload extracts an *int from a JSON payload value (numbers decode as float64).
func intPtrFromPayload(v any) *int {
	switch t := v.(type) {
	case float64:
		i := int(t)
		return &i
	case int:
		return &t
	case int64:
		i := int(t)
		return &i
	}
	return nil
}

// floatPtrFromPayload extracts a *float64 from a JSON payload value (numbers decode as float64).
func floatPtrFromPayload(v any) *float64 {
	switch t := v.(type) {
	case float64:
		return &t
	case float32:
		f := float64(t)
		return &f
	case int:
		f := float64(t)
		return &f
	}
	return nil
}

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
		// A panic inside a NATS subscriber goroutine crashes the whole pod. Recover here so a
		// single poison event (or an unexpected nil) is logged + terminated instead of taking
		// the service down. Term() (not Nak) drops the message so it is NOT redelivered into a
		// crash loop.
		defer func() {
			if rec := recover(); rec != nil {
				h.logger.Error("catalog sync: handler panicked — dropping event",
					zap.Any("panic", rec), zap.ByteString("data", msg.Data))
				_ = msg.Term()
			}
		}()
		evt, err := sharedevents.FromJSON(msg.Data)
		if err != nil {
			h.logger.Error("catalog sync: unmarshal event failed", zap.Error(err))
			_ = msg.Nak()
			return
		}
		var handleErr error
		switch evt.EventType {
		case "bundle.created", "bundle.updated":
			handleErr = h.syncBundle(context.Background(), evt)
		default:
			handleErr = h.syncCatalogItem(context.Background(), evt)
		}
		if handleErr != nil {
			h.logger.Error("catalog sync: handler failed",
				zap.String("event_type", evt.EventType), zap.Error(handleErr))
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
		{"inventory.bundle.created", "pos-inv-bundle-created"},
		{"inventory.bundle.updated", "pos-inv-bundle-updated"},
	}
	for _, s := range subs {
		// Multi-layer rebind: settle buffer + retry-on-"already bound" so a
		// restart never silently drops the inventory->POS catalog sync.
		sharedevents.SubscribeQueueWithRebind(h.logger, js, "inventory", s.subject, s.durable, handler,
			nats.Durable(s.durable),
			nats.AckExplicit(),
			nats.AckWait(30*time.Second),
			nats.MaxDeliver(5),
			nats.DeliverAll(),
		)
	}

	h.logger.Info("inventory catalog sync subscriptions active",
		zap.String("subjects", "inventory.item.created/updated, inventory.bundle.created/updated"))
	return nil
}

// syncCatalogItem upserts the POSCatalogOverride projection for an inventory item.
// Item name/description/image are always fetched fresh from inventory-api at request time;
// this row stores POS-relevant flags + the synced reference data (use_case, tax, compliance)
// so hospitality items (rooms/facilities/amenities/services) and others project into POS.
func (h *InventoryEventHandler) syncCatalogItem(ctx context.Context, evt *sharedevents.Event) error {
	sku, _ := evt.Payload["sku"].(string)
	itemType, _ := evt.Payload["type"].(string)
	if sku == "" {
		return nil
	}

	// Skip non-sellable inventory types — only GOODS, RECIPE, SERVICE, VOUCHER belong in POS
	if itemType == "INGREDIENT" || itemType == "EQUIPMENT" {
		return nil
	}

	if evt.TenantID.String() == "00000000-0000-0000-0000-000000000000" {
		return nil
	}
	tenantID := evt.TenantID

	requiresAgeVerification, _ := evt.Payload["requires_age_verification"].(bool)
	isControlledSubstance, _ := evt.Payload["is_controlled_substance"].(bool)
	isActive, _ := evt.Payload["is_active"].(bool)
	useCase, _ := evt.Payload["use_case"].(string)
	taxCodeID, _ := evt.Payload["tax_code_id"].(string)

	itemID := uuidFromPayload(evt.Payload["id"])
	durationMinutes := intPtrFromPayload(evt.Payload["duration_minutes"])
	// Cache the inventory cost so POS-side profitability reports (Most-Profitable) can compute real
	// margins without an S2S call. Stored in metadata.cost_price; selling_price stays the price override.
	costPrice := floatPtrFromPayload(evt.Payload["cost_price"])

	existing, _ := h.client.POSCatalogOverride.Query().
		Where(entoverride.TenantID(tenantID), entoverride.InventorySku(sku)).
		First(ctx)

	if existing != nil {
		upd := existing.Update().
			SetRequiresAgeVerification(requiresAgeVerification).
			SetIsControlledSubstance(isControlledSubstance).
			SetIsAvailable(isActive).
			SetItemUseCase(useCase)
		if itemID != nil {
			upd = upd.SetInventoryItemID(*itemID)
		}
		if taxCodeID != "" {
			upd = upd.SetTaxCodeID(taxCodeID)
		}
		if durationMinutes != nil {
			upd = upd.SetDurationMinutes(*durationMinutes)
		}
		if costPrice != nil {
			md := existing.Metadata
			if md == nil {
				md = map[string]any{}
			}
			md["cost_price"] = *costPrice
			upd = upd.SetMetadata(md)
		}
		if _, err := upd.Save(ctx); err != nil {
			return fmt.Errorf("update catalog override for %s: %w", sku, err)
		}
		h.logger.Debug("POS catalog override updated", zap.String("sku", sku), zap.String("use_case", useCase))
		return nil
	}

	// Create the projection row so the item is sellable in POS without a manual price step.
	// selling_price stays nil → POS falls back to inventory-api pricing tiers.
	create := h.client.POSCatalogOverride.Create().
		SetTenantID(tenantID).
		SetInventorySku(sku).
		SetItemUseCase(useCase).
		SetRequiresAgeVerification(requiresAgeVerification).
		SetIsControlledSubstance(isControlledSubstance).
		SetIsAvailable(isActive)
	if itemID != nil {
		create = create.SetInventoryItemID(*itemID)
	}
	if taxCodeID != "" {
		create = create.SetTaxCodeID(taxCodeID)
	}
	if durationMinutes != nil {
		create = create.SetDurationMinutes(*durationMinutes)
	}
	if costPrice != nil {
		create = create.SetMetadata(map[string]any{"cost_price": *costPrice})
	}
	if _, err := create.Save(ctx); err != nil {
		return fmt.Errorf("create catalog override for %s: %w", sku, err)
	}
	h.logger.Debug("POS catalog override created", zap.String("sku", sku), zap.String("use_case", useCase))
	return nil
}

// syncBundle marks the catalog projection of a bundle's parent item as a package
// (conference DDR/RDR, room rate plan, service-session bundle).
func (h *InventoryEventHandler) syncBundle(ctx context.Context, evt *sharedevents.Event) error {
	if evt.TenantID.String() == "00000000-0000-0000-0000-000000000000" {
		return nil
	}
	tenantID := evt.TenantID
	sku, _ := evt.Payload["sku"].(string)
	itemID := uuidFromPayload(evt.Payload["item_id"])
	packageType, _ := evt.Payload["package_type"].(string)
	isActive, _ := evt.Payload["is_active"].(bool)

	q := h.client.POSCatalogOverride.Query().Where(entoverride.TenantID(tenantID))
	switch {
	case sku != "":
		q = q.Where(entoverride.InventorySku(sku))
	case itemID != nil:
		q = q.Where(entoverride.InventoryItemID(*itemID))
	default:
		return nil
	}
	existing, _ := q.First(ctx)
	if existing == nil {
		// Parent item event not yet projected; nothing to flag. Item event will create the row.
		h.logger.Debug("bundle sync: no catalog row yet", zap.String("sku", sku))
		return nil
	}
	upd := existing.Update().SetIsBundle(true).SetIsAvailable(isActive)
	md := existing.Metadata
	if md == nil {
		md = map[string]any{}
	}
	md["package_type"] = packageType
	upd = upd.SetMetadata(md)
	if _, err := upd.Save(ctx); err != nil {
		return fmt.Errorf("flag bundle override: %w", err)
	}
	h.logger.Debug("POS catalog bundle flagged", zap.String("sku", sku), zap.String("package_type", packageType))
	return nil
}
