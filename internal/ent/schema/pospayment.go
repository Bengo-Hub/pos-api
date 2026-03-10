package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// POSPayment holds the schema definition for the POSPayment entity.
type POSPayment struct {
	ent.Schema
}

// Fields of the POSPayment.
func (POSPayment) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("order_id", uuid.UUID{}),
		field.UUID("tender_id", uuid.UUID{}),
		field.Float("amount"),
		field.String("currency").
			Default("KES"),
		field.String("status").
			Default("completed"),
		field.String("external_reference").
			Optional(),
		field.JSON("payment_data", map[string]any{}).
			Optional(),
		field.Time("occurred_at").
			Default(time.Now),
	}
}

// Edges of the POSPayment.
func (POSPayment) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("order", POSOrder.Type).
			Ref("payments").
			Field("order_id").
			Unique().
			Required(),
	}
}
