package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// StaffPayroll holds a payroll run for a single staff member over a period.
type StaffPayroll struct {
	ent.Schema
}

func (StaffPayroll) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("staff_id", uuid.UUID{}).Comment("FK to StaffMember"),
		field.Time("period_start"),
		field.Time("period_end"),
		field.Float("gross_amount").Default(0),
		field.Float("total_deductions").Default(0),
		field.Float("net_amount").Default(0),
		field.String("currency").Default("KES"),
		field.Enum("status").
			Values("draft", "approved", "paid", "cancelled").
			Default("draft"),
		field.UUID("approved_by", uuid.UUID{}).Optional().Nillable(),
		field.Time("approved_at").Optional().Nillable(),
		field.Time("paid_at").Optional().Nillable(),
		field.String("payout_reference").Optional().Nillable().
			Comment("Treasury payout reference or M-Pesa transaction code"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (StaffPayroll) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("lines", StaffPayrollLine.Type),
	}
}

func (StaffPayroll) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "staff_id"),
		index.Fields("tenant_id", "status"),
		index.Fields("tenant_id", "period_start", "period_end"),
	}
}
