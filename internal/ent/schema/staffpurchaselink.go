package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// StaffPurchaseLink is pos-api's local record of a staff purchase (layaway or credit sale) that a
// staff member funds from salary. pos-api pushes it to erp-api (which creates the payroll
// recoverable) and tracks settlement as erp recovers installments via erp.staff_purchase.recovered.
type StaffPurchaseLink struct {
	ent.Schema
}

func (StaffPurchaseLink) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("staff_member_id", uuid.UUID{}).Comment("FK to StaffMember"),
		field.UUID("user_id", uuid.UUID{}).Comment("Auth-service user id (the sync key to erp)"),

		field.Enum("origin").Values("layaway", "credit_sale"),
		field.UUID("layaway_plan_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("pos_order_id", uuid.UUID{}).Optional().Nillable(),

		field.String("source_key").NotEmpty().Comment("pos:<origin>:<id> — idempotency key (unique per tenant)"),
		field.UUID("erp_purchase_id", uuid.UUID{}).Optional().Nillable().Comment("erp StaffPurchaseDeduction id once synced"),

		field.Float("principal").GoType(decimal.Decimal{}),
		field.Float("amount_settled").GoType(decimal.Decimal{}).Comment("Recovered so far via payroll"),
		field.Float("outstanding").GoType(decimal.Decimal{}),

		field.Enum("sync_status").Values("pending", "synced", "failed").Default("pending"),
		field.Enum("status").Values("active", "settled", "cancelled").Default("active"),
		field.String("sync_error").Optional(),

		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (StaffPurchaseLink) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id"),
		index.Fields("tenant_id", "staff_member_id"),
		index.Fields("tenant_id", "source_key").Unique(),
	}
}
