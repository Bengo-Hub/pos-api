package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// LoyaltyTransaction holds the schema definition for the LoyaltyTransaction entity.
type LoyaltyTransaction struct{ ent.Schema }

func (LoyaltyTransaction) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("account_id", uuid.UUID{}),
		field.UUID("order_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("outlet_id", uuid.UUID{}).Optional().Nillable(),
		field.String("type_field").Comment("earn, redeem, adjust, expire"),
		field.Int("points").Comment("Positive for earn, negative for redeem/expire"),
		field.Int("balance_after"),
		field.String("notes").Optional(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (LoyaltyTransaction) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("account_id"),
		index.Fields("tenant_id", "order_id"),
	}
}
