package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// CommissionRule defines how commission is calculated for a staff member and/or catalog item.
type CommissionRule struct {
	ent.Schema
}

func (CommissionRule) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("staff_member_id", uuid.UUID{}).Optional().Nillable().Comment("null = applies to all staff"),
		field.UUID("catalog_item_id", uuid.UUID{}).Optional().Nillable().Comment("null = applies to all services"),
		field.String("rule_type").Default("percentage").Comment("flat | percentage | tiered"),
		field.Float("flat_amount").Optional().Nillable().Comment("Fixed commission amount"),
		field.Float("percentage").Optional().Nillable().Comment("Commission rate as percentage (0-100)"),
		field.JSON("tier_rules", []map[string]any{}).Optional().Comment("[{min_sales, max_sales, rate}]"),
		field.Bool("is_active").Default(true),
		field.Time("effective_from").Default(time.Now),
		field.Time("effective_to").Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (CommissionRule) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "is_active"),
		index.Fields("tenant_id", "staff_member_id"),
		index.Fields("tenant_id", "catalog_item_id"),
	}
}
