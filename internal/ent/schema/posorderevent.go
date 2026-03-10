package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// POSOrderEvent holds the schema definition for the POSOrderEvent entity.
type POSOrderEvent struct {
	ent.Schema
}

// Fields of the POSOrderEvent.
func (POSOrderEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("order_id", uuid.UUID{}),
		field.String("event_type").
			NotEmpty(),
		field.UUID("actor_id", uuid.UUID{}).
			Optional(),
		field.JSON("payload", map[string]any{}).
			Default(map[string]any{}),
		field.Time("occurred_at").
			Default(time.Now),
	}
}

// Edges of the POSOrderEvent.
func (POSOrderEvent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("order", POSOrder.Type).
			Ref("events").
			Field("order_id").
			Unique().
			Required(),
	}
}
