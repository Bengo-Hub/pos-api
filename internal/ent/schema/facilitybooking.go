package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// FacilityBooking represents a session booking for a hotel facility.
type FacilityBooking struct {
	ent.Schema
}

// Fields of the FacilityBooking.
func (FacilityBooking) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("facility_id", uuid.UUID{}),
		field.UUID("room_guest_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Hotel guest reference — nil for walk-in bookings"),
		field.String("guest_name").
			NotEmpty(),
		field.String("phone").
			NotEmpty(),
		field.Time("session_date"),
		field.String("start_time").
			Comment("HH:MM format"),
		field.String("end_time").
			Comment("HH:MM format"),
		field.Int("guests_count").
			Default(1).
			Min(1),
		field.Float("amount").
			Min(0),
		field.String("currency").
			Default("KES"),
		field.Enum("status").
			Values("confirmed", "cancelled", "completed").
			Default("confirmed"),
		field.UUID("booked_by", uuid.UUID{}).
			Comment("user_id ref from auth-service"),
		field.String("notes").
			Optional(),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Edges of the FacilityBooking.
func (FacilityBooking) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("facility", Facility.Type).Ref("bookings").Field("facility_id").Unique().Required(),
	}
}

// Indexes of the FacilityBooking.
func (FacilityBooking) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "facility_id"),
		index.Fields("tenant_id", "session_date", "status"),
	}
}
