package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// CashDrawerEvent holds the schema definition for the CashDrawerEvent entity.
type CashDrawerEvent struct {
	ent.Schema
}

// Fields of the CashDrawerEvent.
func (CashDrawerEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("drawer_id", uuid.UUID{}),
		field.String("event_type").
			NotEmpty(),
		field.Float("amount").
			Default(0),
		field.String("reason").
			Optional(),
		field.Time("occurred_at").
			Default(time.Now),
	}
}

// Edges of the CashDrawerEvent.
func (CashDrawerEvent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("drawer", CashDrawer.Type).
			Ref("events").
			Field("drawer_id").
			Unique().
			Required(),
	}
}
