package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// StaffAdvance records a cash advance issued to a staff member,
// to be repaid via payroll deductions.
type StaffAdvance struct {
	ent.Schema
}

func (StaffAdvance) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("staff_id", uuid.UUID{}).Comment("FK to StaffMember"),
		field.Float("amount").Comment("Gross advance amount"),
		field.String("currency").Default("KES"),
		field.Text("reason").Optional(),
		field.Int("repayment_months").Default(1).
			Comment("Number of payroll periods over which to deduct the advance"),
		field.Enum("status").
			Values("active", "fully_repaid", "cancelled").
			Default("active"),
		field.UUID("approved_by", uuid.UUID{}).Optional().Nillable().
			Comment("Staff member who approved the advance"),
		field.Time("approved_at").Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (StaffAdvance) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "staff_id"),
		index.Fields("tenant_id", "status"),
	}
}
