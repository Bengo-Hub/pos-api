package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// RoomBooking is the group/multi-room header for a hotel reservation.
// One RoomBooking owns many RoomGuest rows (one per occupied room), so the
// "number of rooms" requirement is captured at the booking level.
type RoomBooking struct {
	ent.Schema
}

// Fields of the RoomBooking.
func (RoomBooking) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.String("confirmation_no").
			NotEmpty().
			Comment("Human-friendly booking reference, unique per tenant"),
		field.String("lead_guest_name").
			NotEmpty(),
		field.String("email").
			Optional(),
		field.String("phone").
			Optional(),
		field.Int("rooms_count").
			Default(1).
			Min(1).
			Comment("Number of rooms in this booking"),
		field.Time("arrival_date"),
		field.Time("departure_date"),
		field.UUID("inventory_rate_plan_bundle_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Ref to inventory-api Bundle (ROOM_RATE_PLAN) applied to this booking"),
		field.String("market_segment").
			Optional().
			Comment("e.g. corporate, ota, walk_in, group"),
		field.Enum("source").
			Values("staff", "online", "api").
			Default("staff"),
		field.UUID("crm_contact_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("marketflow-api CRM contact ref — never duplicate contact master data here"),
		field.Enum("status").
			Values("confirmed", "checked_in", "checked_out", "cancelled", "no_show").
			Default("confirmed"),
		field.UUID("created_by", uuid.UUID{}).
			Comment("user_id ref from auth-service"),
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

// Edges of the RoomBooking.
func (RoomBooking) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("guests", RoomGuest.Type),
	}
}

// Indexes of the RoomBooking.
func (RoomBooking) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "confirmation_no").Unique(),
		index.Fields("tenant_id", "outlet_id", "status"),
		index.Fields("tenant_id", "arrival_date"),
	}
}
