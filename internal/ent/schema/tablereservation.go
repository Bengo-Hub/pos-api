package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// TableReservation holds future guest reservations for a specific table.
type TableReservation struct {
	ent.Schema
}

func (TableReservation) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("table_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Nil means any available table; set on confirmation"),
		field.String("guest_name").
			NotEmpty(),
		field.String("guest_phone").
			Optional().
			Nillable(),
		field.String("guest_email").
			Optional().
			Nillable(),
		field.Int("party_size").
			Default(2),
		field.Time("scheduled_at").
			Comment("Reservation start time"),
		field.Int("duration_minutes").
			Default(90).
			Comment("Intended table occupancy duration"),
		field.Enum("status").
			Values("pending", "confirmed", "checked_in", "cancelled", "no_show").
			Default("pending"),
		field.String("notes").
			Optional().
			Nillable(),
		field.String("special_requests").
			Optional().
			Nillable(),
		field.String("source").
			Default("staff").
			Comment("staff | phone | online_widget"),
		field.String("cancellation_reason").
			Optional().
			Nillable(),
		field.Time("confirmed_at").
			Optional().
			Nillable(),
		field.Time("checked_in_at").
			Optional().
			Nillable(),
		field.Time("cancelled_at").
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

func (TableReservation) Indexes() []ent.Index {
	return []ent.Index{
		// Fast lookup: all reservations for a tenant+outlet on a given day
		index.Fields("tenant_id", "outlet_id", "scheduled_at"),
		// Fast lookup: all reservations for a specific table
		index.Fields("table_id", "scheduled_at"),
		// Status filter
		index.Fields("tenant_id", "status"),
	}
}
