package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// LoyaltyProgram holds the schema definition for the LoyaltyProgram entity.
type LoyaltyProgram struct{ ent.Schema }

func (LoyaltyProgram) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("name").NotEmpty(),
		field.String("description").Optional(),
		field.Float("earn_rate").Default(1.0).Comment("Points per unit of currency"),
		field.Float("redeem_rate").Default(0.01).Comment("Currency value per point"),
		field.Int("min_redeem_points").Default(100),
		field.Bool("is_active").Default(true),
		field.JSON("tier_thresholds", map[string]any{}).Optional().Comment("Tier name → min lifetime points, e.g. {\"silver\":500,\"gold\":2000}"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (LoyaltyProgram) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id"),
	}
}
