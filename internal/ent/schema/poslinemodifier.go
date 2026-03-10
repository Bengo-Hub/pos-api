package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// POSLineModifier holds the schema definition for the POSLineModifier entity.
type POSLineModifier struct {
	ent.Schema
}

// Fields of the POSLineModifier.
func (POSLineModifier) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("line_id", uuid.UUID{}),
		field.UUID("modifier_id", uuid.UUID{}),
		field.String("name").
			NotEmpty(),
		field.Float("price_applied"),
	}
}

// Edges of the POSLineModifier.
func (POSLineModifier) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("line", POSOrderLine.Type).
			Ref("modifiers").
			Field("line_id").
			Unique().
			Required(),
	}
}
