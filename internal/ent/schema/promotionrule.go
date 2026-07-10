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
			Values("percentage", "fixed_amount", "fixed_price", "bogo").
			Default("percentage"),
		field.Float("discount_value").
			Default(0),
		// BOGO ("buy X get Y [at N% off]") fields — only meaningful when discount_type=bogo.
		// For every buy_quantity units of a scoped SKU in the cart, get_quantity more units of
		// the SAME SKU are discounted by get_discount_percent (100 = fully free, the classic
		// "buy one get one free"; a lower value covers "buy one get one half price" etc.).
		field.Int("buy_quantity").
			Default(1).
			Comment("BOGO: units of the scoped item that must be bought to trigger the deal"),
		field.Int("get_quantity").
			Default(1).
			Comment("BOGO: units of the scoped item discounted per buy_quantity bought"),
		field.Float("get_discount_percent").
			Default(100).
			Comment("BOGO: how much of the \"get\" units' price is discounted (100 = free)"),
		field.Enum("meal_period").
			Values("breakfast", "am_break", "lunch", "pm_break", "dinner").
			Optional().
			Nillable().
			Comment("When set, the discount targets a specific meal period (negotiated lunch/dinner rate, etc.)"),
		field.Float("max_discount").
			Optional().
			Nillable().
			Comment("Cap on the computed discount amount"),
		field.JSON("rule_config", map[string]any{}).
			Default(map[string]any{}),
	}
}
