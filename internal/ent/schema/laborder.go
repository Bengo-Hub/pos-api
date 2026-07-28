package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// LabOrder is a batch of tests requested for a PatientVisit at examination time — results post
// back to the ExaminationRecord once every line completes.
type LabOrder struct{ ent.Schema }

func (LabOrder) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("visit_id", uuid.UUID{}),
		field.UUID("examination_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("ordered_by", uuid.UUID{}),
		// awaiting_payment is the initial state when the outlet requires lab pre-payment: the Lab
		// module ignores the order until the linked bill is settled, at which point it flips to
		// "ordered". Outlets with prepayment off start straight at "ordered".
		field.Enum("status").
			Values("awaiting_payment", "ordered", "in_progress", "completed", "cancelled").
			Default("ordered"),
		// The bill the patient pays before testing starts — created through the SAME order/payment
		// path every POS sale uses (no second billing system), one line per requested test.
		field.UUID("payment_order_id", uuid.UUID{}).Optional().Nillable(),
		field.Float("total_amount").GoType(decimal.Decimal{}).Optional(),
		field.String("notes").Optional(),
		field.Time("ordered_at").Default(time.Now).Immutable(),
		field.Time("completed_at").Optional().Nillable(),
	}
}

func (LabOrder) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "visit_id"),
		index.Fields("tenant_id", "status"),
	}
}
