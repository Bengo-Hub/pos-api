package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// CatalogItem holds the schema definition for the CatalogItem entity.
type CatalogItem struct {
	ent.Schema
}

// Fields of the CatalogItem.
func (CatalogItem) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("name").
			NotEmpty(),
		field.String("description").
			Optional(),
		field.String("sku").
			NotEmpty(),
		field.String("barcode").
			Optional(),
		field.String("category").
			Optional(),
		field.String("image_url").
			Optional(),
		field.String("tax_status").
			Default("taxable"),
		field.String("status").
			Default("active"),
		field.UUID("inventory_item_id", uuid.UUID{}).Optional().Nillable().Comment("FK to inventory master item for sync"),
		field.String("item_type").Optional().Comment("GOODS, SERVICE, RECIPE, etc. synced from inventory"),
		field.Bool("requires_age_verification").Default(false).Comment("Synced from inventory — liquor, tobacco, 18+"),
		field.Bool("is_controlled_substance").Default(false).Comment("Synced from inventory — pharmacy scheduled drugs"),
		field.Bool("track_serial_number").Default(false).Comment("Synced from inventory — electronics, equipment"),
		field.Int("duration_minutes").Optional().Nillable().Comment("Service duration for salon/appointment flow"),
		field.Float("cost_price").Optional().Nillable().Comment("Cost for margin analysis"),
		field.JSON("tags", []string{}).Default([]string{}).Comment("Synced dietary/custom tags from inventory"),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the CatalogItem.
func (CatalogItem) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "inventory_item_id"),
		index.Fields("tenant_id", "sku"),
	}
}

// Edges of the CatalogItem.
func (CatalogItem) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("price_book_items", PriceBookItem.Type),
	}
}
