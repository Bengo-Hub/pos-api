package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// OutboxEvent holds the schema definition for the OutboxEvent entity.
type OutboxEvent struct {
	ent.Schema
}

// Fields of the OutboxEvent.
func (OutboxEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Tenant ID for scoping; uuid.Nil for global events"),
		field.String("aggregate_type").
			NotEmpty(),
		field.String("aggregate_id").
			NotEmpty(),
		field.String("event_type").
			NotEmpty(),
		field.JSON("payload", []byte{}).
			Comment("Serialized event payload"),
		field.String("status").
			Default("PENDING").
			Comment("PENDING | PUBLISHED | FAILED"),
		field.Int("attempts").
			Default(0),
		field.Time("last_attempt_at").
			Optional().
			Nillable(),
		field.Time("published_at").
			Optional().
			Nillable(),
		field.Text("error_message").
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Edges of the OutboxEvent.
func (OutboxEvent) Edges() []ent.Edge {
	return nil
}

// Indexes of the OutboxEvent.
func (OutboxEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status"),
		index.Fields("created_at"),
		index.Fields("tenant_id", "status"),
	}
}
