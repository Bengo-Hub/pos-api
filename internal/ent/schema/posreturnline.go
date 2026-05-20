package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// POSReturnLine is one line item within a POSReturn.
type POSReturnLine struct {
	ent.Schema
}

func (POSReturnLine) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("return_id", uuid.UUID{}),
		field.UUID("order_line_id", uuid.UUID{}).
			Comment("Original POSOrderLine being returned"),
		field.String("sku").
			Optional(),
		field.String("name"),
		field.Float("quantity"),
		field.Float("unit_price"),
		field.Float("total_price"),
		field.String("reason").
			Optional(),
	}
}

func (POSReturnLine) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("return", POSReturn.Type).
			Ref("lines").
			Field("return_id").
			Unique().
			Required(),
	}
}
