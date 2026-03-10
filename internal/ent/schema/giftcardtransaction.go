package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// GiftCardTransaction holds the schema definition for the GiftCardTransaction entity.
type GiftCardTransaction struct {
	ent.Schema
}

// Fields of the GiftCardTransaction.
func (GiftCardTransaction) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("gift_card_id", uuid.UUID{}),
		field.String("transaction_type").
			NotEmpty().
			Comment("load | spend | refund | void"),
		field.Float("amount"),
		field.UUID("reference_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Reference to Order or Payment"),
		field.Time("occurred_at").
			Default(time.Now),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
	}
}
