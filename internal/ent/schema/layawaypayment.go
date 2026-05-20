package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// LayawayPayment holds the schema definition for the LayawayPayment entity.
type LayawayPayment struct{ ent.Schema }

func (LayawayPayment) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("layaway_plan_id", uuid.UUID{}),
		field.UUID("tenant_id", uuid.UUID{}),
		field.Float("amount").GoType(decimal.Decimal{}),
		field.String("payment_method").Default("cash").Comment("cash, mpesa, card"),
		field.String("reference").Optional().Comment("M-Pesa reference or card auth code"),
		field.String("notes").Optional(),
		field.UUID("recorded_by", uuid.UUID{}).Optional().Nillable().Comment("Staff who recorded payment"),
		field.Time("paid_at").Default(time.Now),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (LayawayPayment) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("layaway_plan_id"),
		index.Fields("tenant_id"),
	}
}
