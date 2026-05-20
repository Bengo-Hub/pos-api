package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// LayawayPlan holds the schema definition for the LayawayPlan entity.
type LayawayPlan struct{ ent.Schema }

func (LayawayPlan) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("order_id", uuid.UUID{}).Optional().Nillable().Comment("Linked POS order"),
		field.String("customer_name").NotEmpty(),
		field.String("customer_phone").Optional(),
		field.String("customer_email").Optional(),
		field.Float("total_amount").GoType(decimal.Decimal{}),
		field.Float("deposit_amount").GoType(decimal.Decimal{}).Comment("Initial deposit paid"),
		field.Float("paid_amount").GoType(decimal.Decimal{}),
		field.Float("remaining_amount").GoType(decimal.Decimal{}),
		field.String("status").Default("active").Comment("active, completed, cancelled, forfeited"),
		field.String("notes").Optional(),
		field.Time("due_date").Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (LayawayPlan) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id"),
		index.Fields("outlet_id"),
		index.Fields("status"),
		index.Fields("order_id"),
	}
}
