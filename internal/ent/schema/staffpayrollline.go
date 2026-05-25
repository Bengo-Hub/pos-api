package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// StaffPayrollLine is one line item in a StaffPayroll (salary, advance deduction, bonus, etc.).
type StaffPayrollLine struct {
	ent.Schema
}

func (StaffPayrollLine) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("payroll_id", uuid.UUID{}).Comment("FK to StaffPayroll"),
		field.Enum("line_type").
			Values("salary", "bonus", "commission", "advance_repayment", "tax", "nhif", "nssf", "deduction").
			Comment("Type of payroll line"),
		field.String("description").NotEmpty(),
		field.Float("amount"),
		field.UUID("advance_id", uuid.UUID{}).Optional().Nillable().
			Comment("FK to StaffAdvance if this line repays an advance"),
	}
}

func (StaffPayrollLine) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("payroll", StaffPayroll.Type).
			Ref("lines").
			Field("payroll_id").
			Unique().
			Required(),
	}
}

func (StaffPayrollLine) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("payroll_id"),
	}
}
