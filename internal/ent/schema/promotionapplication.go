package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// PromotionApplication holds the schema definition for the PromotionApplication entity.
type PromotionApplication struct {
	ent.Schema
}

// Fields of the PromotionApplication.
func (PromotionApplication) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("promotion_id", uuid.UUID{}),
		field.UUID("order_id", uuid.UUID{}),
		field.Float("discount_amount"),
		field.Time("applied_at").
			Default(time.Now),
	}
}
