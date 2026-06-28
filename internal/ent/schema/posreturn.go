package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// POSReturn models a structured return/exchange against an original order.
type POSReturn struct {
	ent.Schema
}

func (POSReturn) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("order_id", uuid.UUID{}).
			Comment("Original order being returned"),
		field.String("return_number").
			NotEmpty().
			Unique(),
		field.Enum("return_type").
			Values("refund", "exchange", "store_credit").
			Default("refund"),
		field.Enum("status").
			Values("pending", "approved", "rejected", "completed").
			Default("pending"),
		field.String("reason").
			Optional(),
		field.Enum("reason_code").
			Values("changed_mind", "defective", "damaged", "wrong_item", "expired", "other").
			Optional().
			Nillable(),
		field.Float("refund_amount").
			Default(0).
			Comment("Monetary amount to refund; 0 for exchange/store_credit"),
		field.UUID("exchange_order_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("New order created for an exchange return"),
		field.UUID("requested_by", uuid.UUID{}).
			Comment("Staff member who initiated the return"),
		field.UUID("approved_by", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Manager who approved/rejected"),
		field.Enum("refund_channel").
			Values("cash", "mpesa", "bank", "cheque", "store_credit", "offset_invoice").
			Optional().
			Nillable().
			Comment("How the refund is settled to the customer; passed to treasury as refund_channel and surfaced as the returns-list refund_method column"),
		field.String("treasury_refund_ref").
			Optional().
			Nillable().
			Comment("Payment processor refund reference from treasury-api"),
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

func (POSReturn) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("lines", POSReturnLine.Type),
	}
}

func (POSReturn) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "return_number").Unique(),
		index.Fields("tenant_id", "order_id"),
	}
}
