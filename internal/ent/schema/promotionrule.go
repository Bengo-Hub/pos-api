package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// PromotionRule holds the schema definition for the PromotionRule entity.
type PromotionRule struct {
	ent.Schema
}

// Fields of the PromotionRule.
func (PromotionRule) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("promotion_id", uuid.UUID{}),
		field.String("rule_type").
			NotEmpty(),
		field.JSON("rule_config", map[string]any{}).
			Default(map[string]any{}),
	}
}
