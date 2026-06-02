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
		field.UUID("booking_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("FK to RoomBooking group header (multi-room booking); nil for standalone single-room check-ins"),
		field.String("guest_name").
			NotEmpty(),
		field.String("first_name").
			Optional(),
		field.String("last_name").
			Optional(),
		field.String("email").
			Optional().
			Comment("Guest email for confirmations/folio"),
		field.String("phone").
			NotEmpty(),
		field.String("nationality").
			Optional(),
		field.Enum("id_type").
			Values("national_id", "passport", "driving_licence", "other").
			Default("national_id"),
		field.String("id_number").
			NotEmpty(),
		field.String("id_document_url").
			Optional().
			Comment("Object-storage KEY for the scanned ID document (PII — never store the blob inline)"),
		field.Int("adults").
			Default(1).
			Min(1),
		field.Int("children").
			Default(0).
			Min(0),
		field.JSON("child_ages", []int{}).
			Optional().
			Comment("Ages of accompanying children"),
		field.Enum("source").
			Values("staff", "online", "api").
			Default("staff"),
		field.UUID("crm_contact_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("marketflow-api CRM contact ref — never duplicate contact master data here"),
		field.Time("check_in_date"),
		field.Time("expected_arrival_at").
			Optional().
			Nillable().
			Comment("Planned arrival datetime from the check-in calendar picker (distinct from audit checked_in_at)"),
		field.Int("nights").
			Min(1),
		field.Time("check_out_date"),
		field.Time("expected_departure_at").
			Optional().
			Nillable().
			Comment("Planned departure datetime from the check-out calendar picker"),
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
		field.Bool("late_checkout_approved").
			Default(false),
		field.Float("late_checkout_surcharge").
			Default(0),
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
		edge.From("booking", RoomBooking.Type).Ref("guests").Field("booking_id").Unique(),
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
