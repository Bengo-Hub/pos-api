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
		// Course firing: tracks the highest course number whose items have been sent to the kitchen.
		// KDS shows items with course_number <= fired_courses; higher courses stay hidden until fired.
		field.Int("fired_courses").
			Default(0).
			Comment("Highest course number sent to KDS. 0 = no courses fired (all course_number=0 items fire immediately)."),
		// eTIMS fields — set by treasury.etims.invoice_transmitted NATS subscriber.
		// treasury-api owns eTIMS submission; pos-api only stores the outcome for receipt generation.
		field.String("etims_invoice_number").
			Optional().
			Nillable(),
		field.String("etims_qr_code_url").
			Optional().
			Nillable(),
		// Void fields — set when Admin/Manager voids an order.
		field.String("voided_reason").
			Optional().
			Nillable(),
		field.UUID("voided_by", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Time("voided_at").
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
