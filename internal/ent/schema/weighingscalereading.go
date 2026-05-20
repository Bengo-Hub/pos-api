package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// WeighingScaleReading holds the schema definition for the WeighingScaleReading entity.
type WeighingScaleReading struct{ ent.Schema }

func (WeighingScaleReading) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("session_id", uuid.UUID{}).Optional().Nillable().Comment("POS device session"),
		field.String("device_serial").Optional().Comment("Scale device identifier"),
		field.Float("weight_kg").Comment("Raw weight reading from scale"),
		field.String("unit").Default("kg").Comment("Unit: kg, g, lb"),
		field.UUID("catalog_item_id", uuid.UUID{}).Optional().Nillable().Comment("Item being weighed"),
		field.String("status").Default("captured").Comment("captured, void"),
		field.Time("read_at").Default(time.Now),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (WeighingScaleReading) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id"),
		index.Fields("outlet_id"),
		index.Fields("session_id"),
		index.Fields("catalog_item_id"),
	}
}
