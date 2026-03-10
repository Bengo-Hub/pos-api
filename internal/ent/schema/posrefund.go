package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// POSRefund holds the schema definition for the POSRefund entity.
type POSRefund struct {
	ent.Schema
}

// Fields of the POSRefund.
func (POSRefund) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("order_id", uuid.UUID{}),
		field.UUID("payment_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Float("amount"),
		field.String("reason").
			Optional(),
		field.String("status").
			Default("completed"),
		field.Time("occurred_at").
			Default(time.Now),
	}
}
