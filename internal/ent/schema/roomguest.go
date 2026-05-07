package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// RoomGuest represents a guest stay linked to a hotel room.
type RoomGuest struct {
	ent.Schema
}

// Fields of the RoomGuest.
func (RoomGuest) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("room_id", uuid.UUID{}),
		field.String("guest_name").
			NotEmpty(),
		field.String("phone").
			NotEmpty(),
		field.String("id_number").
			NotEmpty(),
		field.Time("check_in_date"),
		field.Int("nights").
			Min(1),
		field.Time("check_out_date"),
		field.Float("total_room_charge").
			Min(0),
		field.Enum("status").
			Values("active", "checked_out").
			Default("active"),
		field.UUID("checked_in_by", uuid.UUID{}).
			Comment("user_id ref from auth-service"),
		field.UUID("checked_out_by", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Time("checked_in_at").
			Default(time.Now),
		field.Time("checked_out_at").
			Optional().
			Nillable(),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the RoomGuest.
func (RoomGuest) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("room", Room.Type).Ref("guests").Field("room_id").Unique().Required(),
		edge.To("folio_items", RoomFolioItem.Type),
	}
}

// Indexes of the RoomGuest.
func (RoomGuest) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "room_id"),
		index.Fields("tenant_id", "status"),
	}
}
