package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Resource represents a bookable or trackable physical asset in a services outlet
// (e.g. treatment room, massage chair, salon station, consulting room).
type Resource struct {
	ent.Schema
}

func (Resource) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.String("name").NotEmpty().Comment("Display name, e.g. 'Chair 1', 'Treatment Room A'"),
		field.String("type").Default("general").Comment("Category: chair, room, table, equipment, other"),
		field.Enum("status").Values("available", "occupied", "maintenance", "reserved").Default("available"),
		field.String("notes").Optional(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (Resource) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id"),
		index.Fields("tenant_id", "outlet_id", "status"),
		index.Fields("tenant_id", "outlet_id", "type"),
	}
}
