package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
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
		field.Enum("status").
			Values("ordered", "in_progress", "completed", "cancelled").
			Default("ordered"),
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
