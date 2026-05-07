package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// POSOrder holds the schema definition for the POSOrder entity.
type POSOrder struct {
	ent.Schema
}

// Fields of the POSOrder.
func (POSOrder) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("device_id", uuid.UUID{}),
		field.UUID("user_id", uuid.UUID{}),
		field.String("order_number").
			NotEmpty().
			Unique(),
		field.String("status").
			Default("draft"),
		field.Float("subtotal"),
		field.Float("tax_total"),
		field.Float("discount_total").
			Default(0),
		field.Float("total_amount"),
		field.String("currency").
			Default("KES"),
		field.Enum("order_subtype").
			Values("dine_in", "takeaway", "room_service", "delivery", "bar_tab").
			Default("dine_in"),
		field.UUID("room_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Room service: linked room"),
		field.UUID("room_guest_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Room service: linked guest stay"),
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

// Edges of the POSOrder.
func (POSOrder) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("lines", POSOrderLine.Type),
		edge.To("payments", POSPayment.Type),
		edge.To("events", POSOrderEvent.Type),
	}
}

// Indexes of the POSOrder.
func (POSOrder) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "order_number").Unique(),
	}
}
