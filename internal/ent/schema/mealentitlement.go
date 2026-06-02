package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// MealEntitlement is a single meal card / voucher: one row per delegate × conference-day
// × meal-period. Issued from an EventBooking's package; redeemed once at a restaurant/outlet.
type MealEntitlement struct {
	ent.Schema
}

// Fields of the MealEntitlement.
func (MealEntitlement) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("event_booking_id", uuid.UUID{}),
		field.String("delegate_ref").
			Optional().
			Comment("Delegate name/badge number; blank for anonymous per-day count vouchers"),
		field.Time("conference_day").
			Comment("The calendar day this voucher is valid for"),
		field.Enum("meal_period").
			Values("breakfast", "am_break", "lunch", "pm_break", "dinner"),
		field.String("code").
			NotEmpty().
			Comment("Unique redemption code (QR), unique per tenant"),
		field.Time("valid_window_start").
			Optional().
			Nillable(),
		field.Time("valid_window_end").
			Optional().
			Nillable(),
		field.Enum("status").
			Values("issued", "redeemed", "expired", "void").
			Default("issued"),
		field.Time("redeemed_at").
			Optional().
			Nillable(),
		field.UUID("redeemed_outlet_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("redeemed_by", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Staff user_id who redeemed the voucher"),
		field.UUID("pos_order_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("POS order created on redemption (drives meal-BOM backflush)"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the MealEntitlement.
func (MealEntitlement) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("event_booking", EventBooking.Type).
			Ref("meal_entitlements").
			Field("event_booking_id").
			Unique().
			Required(),
	}
}

// Indexes of the MealEntitlement.
func (MealEntitlement) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "code").Unique(),
		index.Fields("tenant_id", "event_booking_id"),
		index.Fields("tenant_id", "status"),
		index.Fields("event_booking_id", "conference_day", "meal_period"),
	}
}
