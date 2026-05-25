package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// KDSSyncFailure records failed KDS event deliveries for DLQ inspection and replay.
type KDSSyncFailure struct {
	ent.Schema
}

func (KDSSyncFailure) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("station_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("order_id", uuid.UUID{}).Optional().Nillable(),
		field.String("event_type").Comment("e.g. kds.ticket.created, kds.order.ready"),
		field.Text("payload").Comment("Raw event payload for replay"),
		field.String("error_message"),
		field.Int("attempt").Default(1),
		field.String("status").Default("failed").Comment("failed | replayed | ignored"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("resolved_at").Optional().Nillable(),
	}
}

func (KDSSyncFailure) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "status"),
		index.Fields("tenant_id", "event_type"),
	}
}
