package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// KDSTicket holds the schema definition for the KDSTicket entity.
type KDSTicket struct {
	ent.Schema
}

// Fields of the KDSTicket.
func (KDSTicket) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("station_id", uuid.UUID{}).Comment("FK to KDSStation"),
		field.UUID("order_id", uuid.UUID{}).Comment("POS or online order reference"),
		field.String("order_number").NotEmpty(),
		field.Enum("status").Values("pending", "in_progress", "ready", "served", "voided").Default("pending"),
		field.JSON("items", []map[string]any{}).Comment("Line items for this station"),
		field.Time("received_at").Default(time.Now),
		field.Time("started_at").Optional().Nillable(),
		field.Time("completed_at").Optional().Nillable(),
		field.Int("priority").Default(0),
	}
}

// Edges of the KDSTicket.
func (KDSTicket) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("station", KDSStation.Type).Ref("tickets").Field("station_id").Unique().Required(),
	}
}

// Indexes of the KDSTicket.
func (KDSTicket) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "station_id", "status"),
		index.Fields("order_id"),
	}
}
