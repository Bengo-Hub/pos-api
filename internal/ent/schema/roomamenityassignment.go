package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// RoomAmenityAssignment links a RoomAmenity to a specific Room (or room type via metadata).
// When a guest checks in, included amenities are auto-charged; chargeable ones are on-demand.
type RoomAmenityAssignment struct {
	ent.Schema
}

func (RoomAmenityAssignment) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("room_id", uuid.UUID{}),
		field.UUID("amenity_id", uuid.UUID{}),
		// Override the amenity's default billing_mode for this specific room
		field.Bool("is_included").Default(false).
			Comment("true = included in room rate; false = charged separately"),
		field.String("notes").Optional(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (RoomAmenityAssignment) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("room", Room.Type).Ref("amenity_assignments").Field("room_id").Unique().Required(),
		edge.From("amenity", RoomAmenity.Type).Ref("assignments").Field("amenity_id").Unique().Required(),
	}
}

func (RoomAmenityAssignment) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("room_id", "amenity_id").Unique(),
		index.Fields("tenant_id", "room_id"),
	}
}
