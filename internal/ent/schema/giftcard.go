package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// GiftCard holds the schema definition for the GiftCard entity.
type GiftCard struct {
	ent.Schema
}

// Fields of the GiftCard.
func (GiftCard) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("card_number").
			Unique().
			NotEmpty(),
		field.Float("initial_balance"),
		field.Float("current_balance"),
		field.String("currency").
			Default("KES"),
		field.String("status").
			Default("active"),
		field.Time("expiry_at").
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}
