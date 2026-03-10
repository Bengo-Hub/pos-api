package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// POSOrderLine holds the schema definition for the POSOrderLine entity.
type POSOrderLine struct {
	ent.Schema
}

// Fields of the POSOrderLine.
func (POSOrderLine) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("order_id", uuid.UUID{}),
		field.UUID("catalog_item_id", uuid.UUID{}),
		field.String("sku").
			NotEmpty(),
		field.String("name").
			NotEmpty(),
		field.Float("quantity"),
		field.Float("unit_price"),
		field.Float("total_price"),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
	}
}

// Edges of the POSOrderLine.
func (POSOrderLine) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("order", POSOrder.Type).
			Ref("lines").
			Field("order_id").
			Unique().
			Required(),
		edge.To("modifiers", POSLineModifier.Type),
	}
}
