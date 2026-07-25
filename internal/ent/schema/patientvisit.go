package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// PatientVisit is one OPD episode of care — Records opens it at registration, and it carries the
// patient through Triage -> Examination -> (optional) Lab -> Pharmacy as one linked journey.
type PatientVisit struct{ ent.Schema }

func (PatientVisit) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("patient_id", uuid.UUID{}),
		field.String("visit_number").NotEmpty().Comment("Sequence-generated (Settings -> Documents)"),
		field.Enum("status").
			Values("registered", "triaged", "in_examination", "awaiting_lab", "lab_complete", "prescribed", "dispensed", "completed", "cancelled").
			Default("registered"),
		// The registration/consultation fee is collected through the SAME order+payment path
		// every POS sale uses (a "service" catalog line) — this just remembers which order paid
		// for this visit's intake, not a second billing system.
		field.UUID("registration_fee_order_id", uuid.UUID{}).Optional().Nillable(),
		field.String("chief_complaint").Optional().Comment("Captured at registration; refined at examination"),
		field.UUID("registered_by", uuid.UUID{}).Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (PatientVisit) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "visit_number").Unique(),
		index.Fields("tenant_id", "patient_id"),
		index.Fields("tenant_id", "status"),
		index.Fields("tenant_id", "outlet_id"),
	}
}
