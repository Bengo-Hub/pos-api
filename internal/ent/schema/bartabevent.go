package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// BarTabEvent holds the schema definition for the BarTabEvent entity.
type BarTabEvent struct {
	ent.Schema
}

// Fields of the BarTabEvent.
func (BarTabEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("bar_tab_id", uuid.UUID{}),
		field.String("event_type").
			NotEmpty(),
		field.UUID("order_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.JSON("payload", map[string]any{}).
			Default(map[string]any{}),
		field.Time("occurred_at").
			Default(time.Now),
	}
}

// Edges of the BarTabEvent.
func (BarTabEvent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("bar_tab", BarTab.Type).
			Ref("events").
			Field("bar_tab_id").
			Unique().
			Required(),
	}
}
