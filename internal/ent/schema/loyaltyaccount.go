package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// LoyaltyAccount holds the schema definition for the LoyaltyAccount entity.
type LoyaltyAccount struct{ ent.Schema }

func (LoyaltyAccount) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("customer_id", uuid.UUID{}).Optional().Nillable(),
		field.String("customer_phone").NotEmpty(),
		field.String("customer_name").NotEmpty(),
		field.Int("points_balance").Default(0),
		field.Int("lifetime_points").Default(0),
		field.UUID("program_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("crm_contact_id", uuid.UUID{}).Optional().Nillable().Comment("MarketFlow CRM contact reference — never duplicate contact data here"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (LoyaltyAccount) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "customer_phone").Unique(),
		index.Fields("tenant_id", "customer_id"),
	}
}
