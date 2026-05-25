package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entcatalogitem "github.com/bengobox/pos-service/internal/ent/catalogitem"
)

// InventoryItemEvent represents an inventory item event from inventory-service.
// JSON fields match the shared-events library Event struct.
type InventoryItemEvent struct {
	ID            string                 `json:"id"`
	TenantID      string                 `json:"tenant_id"`
	AggregateType string                 `json:"aggregate_type"`
	AggregateID   string                 `json:"aggregate_id"`
	EventType     string                 `json:"event_type"`
	Data          map[string]interface{} `json:"payload"`
	Timestamp     string                 `json:"timestamp"`
}

// InventoryEventHandler handles inventory events for POS catalog sync.
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

// SubscribeToInventoryEvents subscribes to inventory-service events via JetStream.
func (h *InventoryEventHandler) SubscribeToInventoryEvents(nc *nats.Conn) error {
	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("pos catalog: jetstream init: %w", err)
	}

	// Ensure the inventory stream exists (inventory-api creates it, but may not be ready at pos-api start)
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

	upsertHandler := func(msg *nats.Msg) {
		var evt InventoryItemEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			h.logger.Error("catalog sync: unmarshal failed", zap.Error(err))
			_ = msg.Nak()
			return
		}
		if err := h.handleItemUpsert(context.Background(), &evt); err != nil {
			h.logger.Error("catalog sync: upsert failed", zap.Error(err))
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
		{"inventory.item.created", "pos-catalog-item-created"},
		{"inventory.item.updated", "pos-catalog-item-updated"},
	}
	for _, s := range subs {
		if _, err := js.Subscribe(s.subject, upsertHandler,
			nats.Durable(s.durable),
			nats.AckExplicit(),
			nats.AckWait(30*time.Second),
			nats.MaxDeliver(5),
			nats.DeliverAll(),
		); err != nil {
			h.logger.Warn("pos catalog: subscribe failed", zap.String("subject", s.subject), zap.Error(err))
		}
	}

	h.logger.Info("inventory catalog sync subscriptions active",
		zap.String("subjects", "inventory.item.created, inventory.item.updated"))
	return nil
}

// InitialSync pulls all items from inventory-api via REST and upserts them locally.
// Called once on startup to catch items created before the event subscriber was deployed.
func (h *InventoryEventHandler) InitialSync(ctx context.Context, inventoryAPIURL, tenantSlug string) {
	url := fmt.Sprintf("%s/v1/%s/inventory/items?type=GOODS,RECIPE", inventoryAPIURL, tenantSlug)
	resp, err := http.Get(url)
	if err != nil {
		h.logger.Warn("initial catalog sync failed: HTTP error", zap.String("url", url), zap.Error(err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		h.logger.Warn("initial catalog sync failed: non-200 status", zap.Int("status", resp.StatusCode))
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.logger.Warn("initial catalog sync failed: read body", zap.Error(err))
		return
	}

	var result struct {
		Data []struct {
			ID                       string  `json:"id"`
			SKU                      string  `json:"sku"`
			Name                     string  `json:"name"`
			Description              string  `json:"description"`
			CategoryID               *string `json:"category_id"`
			CategoryName             string  `json:"category_name"`
			IsActive                 bool    `json:"is_active"`
			ImageURL                 string  `json:"image_url"`
			Type                     string  `json:"type"`
			RequiresAgeVerification  bool    `json:"requires_age_verification"`
			IsControlledSubstance    bool    `json:"is_controlled_substance"`
			TrackSerialNumbers       bool    `json:"track_serial_numbers"`
			DurationMinutes          int     `json:"duration_minutes"`
			Barcode                  string  `json:"barcode"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		h.logger.Warn("initial catalog sync failed: unmarshal", zap.Error(err))
		return
	}

	synced := 0
	for _, item := range result.Data {
		// Resolve tenant ID from the first item or use the slug-based approach
		// For now, we'll try to find the tenant from the POS tenant table
		tenants, _ := h.client.Tenant.Query().All(ctx)
		var tenantID uuid.UUID
		for _, t := range tenants {
			if t.Slug == tenantSlug {
				tenantID = t.ID
				break
			}
		}
		if tenantID == uuid.Nil && len(tenants) > 0 {
			tenantID = tenants[0].ID // Fallback to first tenant
		}
		if tenantID == uuid.Nil {
			h.logger.Warn("initial catalog sync: no tenant found", zap.String("slug", tenantSlug))
			return
		}

		// Check if already exists
		existing, _ := h.client.CatalogItem.Query().
			Where(entcatalogitem.TenantID(tenantID), entcatalogitem.Sku(item.SKU)).
			First(ctx)
		if existing != nil {
			continue
		}

		status := "active"
		if !item.IsActive {
			status = "inactive"
		}

		builder := h.client.CatalogItem.Create().
			SetTenantID(tenantID).
			SetSku(item.SKU).
			SetName(item.Name).
			SetStatus(status).
			SetRequiresAgeVerification(item.RequiresAgeVerification).
			SetIsControlledSubstance(item.IsControlledSubstance).
			SetTrackSerialNumber(item.TrackSerialNumbers)

		if item.Description != "" {
			builder.SetDescription(item.Description)
		}
		if item.ImageURL != "" {
			builder.SetImageURL(item.ImageURL)
		}
		if item.CategoryName != "" {
			builder.SetCategory(item.CategoryName)
		}
		if item.Type != "" {
			builder.SetItemType(item.Type)
		}
		if item.DurationMinutes > 0 {
			builder.SetDurationMinutes(item.DurationMinutes)
		}
		if item.Barcode != "" {
			builder.SetBarcode(item.Barcode)
		}
		if itemID, err := uuid.Parse(item.ID); err == nil && itemID != uuid.Nil {
			builder.SetInventoryItemID(itemID)
		}

		if _, err := builder.Save(ctx); err != nil {
			h.logger.Warn("initial catalog sync: create failed",
				zap.String("sku", item.SKU), zap.Error(err))
			continue
		}
		synced++
	}

	h.logger.Info("initial catalog sync completed", zap.Int("synced", synced), zap.Int("total", len(result.Data)))
}

// handleItemUpsert creates or updates a POS CatalogItem from an inventory event.
func (h *InventoryEventHandler) handleItemUpsert(ctx context.Context, evt *InventoryItemEvent) error {
	tenantID, err := uuid.Parse(evt.TenantID)
	if err != nil {
		return fmt.Errorf("invalid tenant_id: %w", err)
	}

	data := evt.Data
	sku, _ := data["sku"].(string)
	name, _ := data["name"].(string)
	description, _ := data["description"].(string)
	imageURL, _ := data["image_url"].(string)
	isActive, _ := data["is_active"].(bool)
	categoryName, _ := data["category_name"].(string)
	itemType, _ := data["type"].(string)
	inventoryItemIDStr, _ := data["id"].(string)

	// Skip non-sellable inventory types — only GOODS, RECIPE, SERVICE, VOUCHER belong in POS.
	if itemType == "INGREDIENT" || itemType == "EQUIPMENT" {
		h.logger.Debug("skipping non-sellable inventory item",
			zap.String("sku", sku), zap.String("type", itemType))
		return nil
	}
	requiresAgeVerification, _ := data["requires_age_verification"].(bool)
	isControlledSubstance, _ := data["is_controlled_substance"].(bool)
	trackSerialNumbers, _ := data["track_serial_numbers"].(bool)
	durationMinutes, _ := data["duration_minutes"].(float64)
	barcode, _ := data["barcode"].(string)

	// Parse inventory item ID for FK reference
	var inventoryItemID uuid.UUID
	if inventoryItemIDStr != "" {
		inventoryItemID, _ = uuid.Parse(inventoryItemIDStr)
	}

	status := "active"
	if !isActive {
		status = "inactive"
	}

	// Check if item already exists by SKU + tenant
	existing, _ := h.client.CatalogItem.Query().
		Where(
			entcatalogitem.TenantID(tenantID),
			entcatalogitem.Sku(sku),
		).
		First(ctx)

	if existing != nil {
		// Update existing item
		builder := h.client.CatalogItem.UpdateOne(existing).
			SetName(name).
			SetStatus(status).
			SetRequiresAgeVerification(requiresAgeVerification).
			SetIsControlledSubstance(isControlledSubstance).
			SetTrackSerialNumber(trackSerialNumbers)

		if description != "" {
			builder.SetDescription(description)
		}
		if imageURL != "" {
			builder.SetImageURL(imageURL)
		}
		if categoryName != "" {
			builder.SetCategory(categoryName)
		}
		if itemType != "" {
			builder.SetItemType(itemType)
		}
		if inventoryItemID != uuid.Nil {
			builder.SetInventoryItemID(inventoryItemID)
		}
		if durationMinutes > 0 {
			builder.SetDurationMinutes(int(durationMinutes))
		}
		if barcode != "" {
			builder.SetBarcode(barcode)
		}

		if _, err := builder.Save(ctx); err != nil {
			return fmt.Errorf("update catalog item: %w", err)
		}

		h.logger.Info("POS catalog item updated from inventory event",
			zap.String("sku", sku), zap.String("name", name))
		return nil
	}

	// Create new catalog item
	builder := h.client.CatalogItem.Create().
		SetTenantID(tenantID).
		SetSku(sku).
		SetName(name).
		SetStatus(status).
		SetRequiresAgeVerification(requiresAgeVerification).
		SetIsControlledSubstance(isControlledSubstance).
		SetTrackSerialNumber(trackSerialNumbers)

	if description != "" {
		builder.SetDescription(description)
	}
	if imageURL != "" {
		builder.SetImageURL(imageURL)
	}
	if categoryName != "" {
		builder.SetCategory(categoryName)
	}
	if itemType != "" {
		builder.SetItemType(itemType)
	}
	if inventoryItemID != uuid.Nil {
		builder.SetInventoryItemID(inventoryItemID)
	}
	if durationMinutes > 0 {
		builder.SetDurationMinutes(int(durationMinutes))
	}
	if barcode != "" {
		builder.SetBarcode(barcode)
	}

	if _, err := builder.Save(ctx); err != nil {
		return fmt.Errorf("create catalog item: %w", err)
	}

	h.logger.Info("POS catalog item created from inventory event",
		zap.String("sku", sku), zap.String("name", name))
	return nil
}
