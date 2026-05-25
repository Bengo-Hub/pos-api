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
		field.Int("weight_grams").
			Optional().
			Nillable().
			Comment("Weight at sale time for weighed items"),
		field.String("lot_number").
			Optional().
			Comment("Lot/batch number if item tracks lots"),
		field.Time("expiry_date").
			Optional().
			Nillable().
			Comment("Expiry date from lot if applicable"),
		field.String("serial_number").
			Optional().
			Nillable().
			Comment("Serial number captured at point of sale for tracked items"),
		field.Float("partial_units").
			Optional().
			Nillable().
			Comment("Partial pack decimal quantity (e.g. 10 of 30 tablets dispensed)"),
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
