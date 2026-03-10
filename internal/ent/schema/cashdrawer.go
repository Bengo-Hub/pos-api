package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// CashDrawer holds the schema definition for the CashDrawer entity.
type CashDrawer struct {
	ent.Schema
}

// Fields of the CashDrawer.
func (CashDrawer) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("device_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.String("status").
			Default("closed"),
		field.Float("starting_cash").
			Default(0),
		field.Float("ending_cash").
			Optional().
			Nillable(),
		field.Time("opened_at").
			Optional().
			Nillable(),
		field.Time("closed_at").
			Optional().
			Nillable(),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
	}
}

// Edges of the CashDrawer.
func (CashDrawer) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("events", CashDrawerEvent.Type),
	}
}
