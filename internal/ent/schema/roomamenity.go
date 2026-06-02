package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// RoomAmenity defines an amenity available at a property (pool, gym, steam room, wifi, transport, etc.).
// Amenities can be free/included or charged per-session/per-day/per-night.
type RoomAmenity struct {
	ent.Schema
}

func (RoomAmenity) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.String("name").NotEmpty(),
		field.Enum("amenity_type").
			Values("pool", "gym", "steam_room", "sauna", "wifi", "transport",
				"parking", "breakfast", "spa", "golf", "laundry", "minibar",
				"airport_transfer", "room_service_24h", "other").
			Default("other"),
		field.String("description").Optional(),
		// "free" = included at no extra cost; "per_session" / "per_day" / "per_night" = chargeable
		field.Enum("billing_mode").
			Values("free", "per_session", "per_day", "per_night").
			Default("free"),
		field.UUID("inventory_item_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Ref to inventory-api SERVICE Item (use_case=AMENITY) — authoritative amenity & rate master"),
		field.Float("rate").Min(0).Default(0).
			Comment("DEPRECATED as authoritative: rate master lives in inventory-api ItemPricing. Synced/read-through snapshot; kept for transition"),
		field.String("currency").Default("KES"),
		field.Bool("is_active").Default(true),
		field.JSON("metadata", map[string]any{}).Default(map[string]any{}),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (RoomAmenity) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("assignments", RoomAmenityAssignment.Type),
	}
}

func (RoomAmenity) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id"),
		index.Fields("tenant_id", "amenity_type"),
	}
}
