package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// EventBooking is a Banquet Event Order (BEO) — a conference, wedding, party or
// similar event booked against a Facility, optionally priced from an inventory
// Bundle (conference DelegatePackage / DDR / RDR).
type EventBooking struct {
	ent.Schema
}

// Fields of the EventBooking.
func (EventBooking) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("facility_id", uuid.UUID{}).
			Comment("Venue (Facility) booked for the event"),
		field.UUID("inventory_bundle_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("inventory-api Bundle (DDR/RDR/half-board) defining the package & inclusions"),
		field.Enum("event_type").
			Values("conference", "wedding", "party", "anniversary", "meeting", "other").
			Default("conference"),
		field.String("title").
			NotEmpty(),
		field.String("client_name").
			NotEmpty(),
		field.String("contact_phone").
			Optional(),
		field.String("contact_email").
			Optional(),
		field.UUID("crm_contact_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("marketflow-api CRM contact ref — never duplicate contact master data here"),
		field.Time("start_at"),
		field.Time("end_at"),
		field.Int("conference_days").
			Default(1).
			Min(1).
			Comment("Number of conference days (drives meal-card generation)"),
		field.Int("delegate_count").
			Default(0).
			Min(0),
		field.Int("expected_pax").
			Default(0).
			Min(0),
		field.Int("guaranteed_minimum_covers").
			Default(0).
			Min(0).
			Comment("Guaranteed billable covers regardless of actual turnout"),
		field.String("setup_style").
			Optional().
			Comment("theatre/classroom/boardroom/u_shape/cabaret/banquet"),
		field.Float("deposit_amount").
			Default(0).
			Min(0),
		field.Bool("deposit_refundable").
			Default(true),
		field.Float("total_amount").
			Default(0).
			Min(0),
		field.String("currency").
			Default("KES"),
		field.Text("special_requests").
			Optional(),
		field.UUID("master_folio_room_guest_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Optional link to a RoomGuest folio when the organiser is a resident guest"),
		field.Enum("status").
			Values("inquiry", "tentative", "confirmed", "in_progress", "completed", "cancelled").
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

// Edges of the EventBooking.
func (EventBooking) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("meal_entitlements", MealEntitlement.Type),
	}
}

// Indexes of the EventBooking.
func (EventBooking) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id", "status"),
		index.Fields("tenant_id", "facility_id"),
		index.Fields("tenant_id", "start_at"),
	}
}
