package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Room represents a hotel room within an outlet.
type Room struct {
	ent.Schema
}

// Fields of the Room.
func (Room) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.String("room_number").
			NotEmpty(),
		field.String("name").
			NotEmpty(),
		field.Enum("room_type").
			Values("standard", "deluxe", "suite", "presidential", "other").
			Default("standard"),
		field.Int("floor").
			Default(1),
		field.Float("rate_per_night").
			Min(0),
		field.String("currency").
			Default("KES"),
		field.Enum("status").
			Values("available", "occupied", "cleaning", "maintenance", "reserved", "checkout").
			Default("available"),
		field.Bool("is_active").
			Default(true),
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

// Edges of the Room.
func (Room) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("guests", RoomGuest.Type),
		edge.To("folio_items", RoomFolioItem.Type),
	}
}

// Indexes of the Room.
func (Room) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id"),
		index.Fields("tenant_id", "room_number").Unique(),
		index.Fields("tenant_id", "status"),
	}
}
