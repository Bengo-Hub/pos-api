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
		// Typed scoping + discount fields (formalize the previously free-form rule_config).
		field.Enum("scope_type").
			Values("all", "category", "item").
			Default("all").
			Comment("Discount scope: all lines, specific inventory categories, or specific items/skus"),
		field.JSON("scope_ids", []string{}).
			Optional().
			Comment("Inventory category ids or skus the discount applies to (when scope_type != all)"),
		field.Enum("discount_type").
			Values("percentage", "fixed_amount", "fixed_price").
			Default("percentage"),
		field.Float("discount_value").
			Default(0),
		field.Float("max_discount").
			Optional().
			Nillable().
			Comment("Cap on the computed discount amount"),
		field.JSON("rule_config", map[string]any{}).
			Default(map[string]any{}),
	}
}
