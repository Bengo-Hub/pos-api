package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// POSDevice holds the schema definition for the POSDevice entity.
type POSDevice struct {
	ent.Schema
}

// Fields of the POSDevice.
func (POSDevice) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.String("device_code").
			NotEmpty(),
		field.String("device_type").
			Default("terminal"),
		field.String("status").
			Default("inactive"),
		field.String("hardware_fingerprint").
			Optional(),
		field.Time("registered_at").
			Default(time.Now),
		field.Time("last_seen_at").
			Optional().
			Nillable(),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
	}
}

// Edges of the POSDevice.
func (POSDevice) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("outlet", Outlet.Type).
			Ref("devices").
			Field("outlet_id").
			Unique().
			Required(),
		edge.To("sessions", POSDeviceSession.Type),
	}
}

// Indexes of the POSDevice.
func (POSDevice) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "device_code").Unique(),
	}
}
