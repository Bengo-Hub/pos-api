package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// CommissionRecord holds the schema definition for the CommissionRecord entity.
type CommissionRecord struct {
	ent.Schema
}

// Fields of the CommissionRecord.
func (CommissionRecord) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("staff_member_id", uuid.UUID{}).Comment("FK to StaffMember"),
		field.UUID("order_id", uuid.UUID{}).Comment("FK to POSOrder"),
		field.UUID("order_line_id", uuid.UUID{}).Optional().Nillable().Comment("FK to POSOrderLine"),
		field.String("service_sku").NotEmpty(),
		field.Float("sale_amount").Default(0),
		field.Float("commission_rate").Default(0).Comment("Percentage at time of sale"),
		field.Float("commission_amount").Default(0),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

// Indexes of the CommissionRecord.
func (CommissionRecord) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "staff_member_id"),
		index.Fields("tenant_id", "order_id"),
	}
}
