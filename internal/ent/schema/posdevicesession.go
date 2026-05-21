package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// POSDeviceSession holds the schema definition for the POSDeviceSession entity.
type POSDeviceSession struct {
	ent.Schema
}

// Fields of the POSDeviceSession.
func (POSDeviceSession) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("device_id", uuid.UUID{}),
		field.UUID("user_id", uuid.UUID{}),
		field.String("session_status").
			Default("open"),
		field.Time("opened_at").
			Default(time.Now),
		field.Time("closed_at").
			Optional().
			Nillable(),
		field.Float("float_amount").
			Default(0),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
	}
}

// Edges of the POSDeviceSession.
func (POSDeviceSession) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("device", POSDevice.Type).
			Ref("sessions").
			Field("device_id").
			Unique().
			Required(),
		// user_id is a plain reference to the staff user who opened the session.
		// No FK edge so that terminal PIN staff (in staff_members) can open sessions
		// without requiring a corresponding record in the users table.
	}
}
