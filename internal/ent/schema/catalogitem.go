package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
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

// Edges of the CatalogItem.
func (CatalogItem) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("price_book_items", PriceBookItem.Type),
	}
}
