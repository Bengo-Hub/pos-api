package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Facility represents a hotel facility (pool, gym, spa, conference room, etc.).
type Facility struct {
	ent.Schema
}

// Fields of the Facility.
func (Facility) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.String("name").
			NotEmpty(),
		field.Enum("facility_type").
			Values("pool", "gym", "conference", "spa", "kids_area", "other").
			Default("other"),
		field.Int("capacity").
			Default(0).
			Min(0),
		field.UUID("inventory_item_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Ref to inventory-api SERVICE Item (use_case=HOSPITALITY_FACILITY/CONFERENCE) — authoritative facility & rate master"),
		field.Float("rate_per_session").
			Min(0).
			Comment("DEPRECATED as authoritative: rate master lives in inventory-api ItemPricing. Synced/read-through snapshot; kept for transition"),
		field.String("currency").
			Default("KES"),
		field.String("opening_time").
			Default("06:00").
			Comment("HH:MM format"),
		field.String("closing_time").
			Default("22:00").
			Comment("HH:MM format"),
		field.Enum("status").
			Values("available", "occupied", "maintenance", "closed").
			Default("available"),
		field.JSON("setup_styles", []string{}).
			Optional().
			Comment("Supported conference layouts: theatre/classroom/boardroom/u_shape/cabaret/banquet"),
		field.Bool("divisible").
			Default(false).
			Comment("True if the hall can be split into sub-rooms"),
		field.UUID("parent_facility_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Parent hall when this facility is a divisible sub-room"),
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

// Edges of the Facility.
func (Facility) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("bookings", FacilityBooking.Type),
	}
}

// Indexes of the Facility.
func (Facility) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id"),
		index.Fields("tenant_id", "status"),
	}
}
