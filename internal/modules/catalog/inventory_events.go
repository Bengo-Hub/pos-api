package catalog

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entcatalogitem "github.com/bengobox/pos-service/internal/ent/catalogitem"
)

// InventoryItemEvent represents an inventory item event from inventory-service.
type InventoryItemEvent struct {
	ID            string                 `json:"id"`
	TenantID      string                 `json:"tenantId"`
	AggregateType string                 `json:"aggregateType"`
	AggregateID   string                 `json:"aggregateId"`
	EventType     string                 `json:"type"`
	Data          map[string]interface{} `json:"data"`
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

// SubscribeToInventoryEvents subscribes to inventory-service events via NATS.
func (h *InventoryEventHandler) SubscribeToInventoryEvents(nc *nats.Conn) error {
	// Subscribe to inventory.item.created
	_, err := nc.Subscribe("inventory.item.created", func(msg *nats.Msg) {
		var evt InventoryItemEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			h.logger.Error("failed to unmarshal inventory.item.created event", zap.Error(err))
			return
		}

		ctx := context.Background()
		if err := h.handleItemUpsert(ctx, &evt); err != nil {
			h.logger.Error("failed to handle inventory.item.created event", zap.Error(err))
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return fmt.Errorf("pos catalog: subscribe to inventory.item.created: %w", err)
	}

	// Subscribe to inventory.item.updated
	_, err = nc.Subscribe("inventory.item.updated", func(msg *nats.Msg) {
		var evt InventoryItemEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			h.logger.Error("failed to unmarshal inventory.item.updated event", zap.Error(err))
			return
		}

		ctx := context.Background()
		if err := h.handleItemUpsert(ctx, &evt); err != nil {
			h.logger.Error("failed to handle inventory.item.updated event", zap.Error(err))
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return fmt.Errorf("pos catalog: subscribe to inventory.item.updated: %w", err)
	}

	h.logger.Info("inventory event subscriptions active",
		zap.String("subjects", "inventory.item.created, inventory.item.updated"))
	return nil
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
			SetStatus(status)

		if description != "" {
			builder.SetDescription(description)
		}
		if imageURL != "" {
			builder.SetImageURL(imageURL)
		}
		if categoryName != "" {
			builder.SetCategory(categoryName)
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
		SetStatus(status)

	if description != "" {
		builder.SetDescription(description)
	}
	if imageURL != "" {
		builder.SetImageURL(imageURL)
	}
	if categoryName != "" {
		builder.SetCategory(categoryName)
	}

	if _, err := builder.Save(ctx); err != nil {
		return fmt.Errorf("create catalog item: %w", err)
	}

	h.logger.Info("POS catalog item created from inventory event",
		zap.String("sku", sku), zap.String("name", name))
	return nil
}
