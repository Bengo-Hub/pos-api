package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// BarTab holds the schema definition for the BarTab entity.
type BarTab struct {
	ent.Schema
}

// Fields of the BarTab.
func (BarTab) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.String("tab_name").
			NotEmpty(),
		field.String("status").
			Default("open"),
		field.Float("total_amount").
			Default(0),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the BarTab.
func (BarTab) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("events", BarTabEvent.Type),
	}
}
