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
		// Party: an existing customer (loyalty account) OR a staff member. Free-text customer
		// fields are kept as a display snapshot regardless of party type.
		field.Enum("party_type").Values("customer", "staff").Default("customer"),
		field.UUID("staff_member_id", uuid.UUID{}).Optional().Nillable().
			Comment("Set when party_type=staff (FK to StaffMember)"),
		field.UUID("loyalty_account_id", uuid.UUID{}).Optional().Nillable().
			Comment("Set when the customer was picked from a loyalty account"),
		field.Bool("fund_from_salary").Default(false).
			Comment("Staff layaway funded via ERP payroll deduction (premium)"),
		field.String("customer_name").NotEmpty(),
		field.String("customer_phone").Optional(),
		field.String("customer_email").Optional(),
		field.Float("total_amount").GoType(decimal.Decimal{}),
		field.Float("deposit_amount").GoType(decimal.Decimal{}).Comment("Initial deposit paid"),
		field.Float("paid_amount").GoType(decimal.Decimal{}),
		field.Float("remaining_amount").GoType(decimal.Decimal{}),
		field.String("status").Default("active").Comment("active, completed, cancelled, forfeited"),
		// Cart line snapshot captured at create so Complete rebuilds real order lines and the sale
		// posts GL, backflushes stock, and fiscalises eTIMS with real SKUs (not a single opaque
		// LAYAWAY line). Optional — legacy/total-only plans keep the summary-line behaviour.
		field.JSON("items", []map[string]any{}).Optional().
			Comment("Line snapshot [{sku,name,quantity,unit_price,total_price,tax_*}] for completion sync"),
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
