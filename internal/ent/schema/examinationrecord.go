package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ExaminationRecord is the doctor/pharmacist's clinical assessment of a PatientVisit: diagnosis,
// optional lab referral, and (eventually) the resulting Prescription. One per visit — updated in
// place as the visit progresses through awaiting_lab -> completed.
type ExaminationRecord struct{ ent.Schema }

func (ExaminationRecord) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("visit_id", uuid.UUID{}),
		field.UUID("examined_by", uuid.UUID{}).Comment("Doctor/pharmacist staff member"),
		field.String("chief_complaint").Optional(),
		// diagnosis stays the human-readable summary (comma-joined when several were picked) so
		// every existing reader/report keeps working; diagnosis_codes carries the structured
		// multi-select behind it — a visit can legitimately carry a COMBINATION of diagnoses.
		field.String("diagnosis").Optional(),
		field.JSON("diagnosis_codes", []string{}).Optional().Default([]string{}).
			Comment("DiagnosisCatalog names/codes selected for this examination"),
		field.String("clinical_notes").Optional(),
		field.Bool("lab_requested").Default(false),
		field.UUID("prescription_id", uuid.UUID{}).Optional().Nillable(),
		field.Enum("status").
			Values("in_progress", "awaiting_lab", "completed").
			Default("in_progress"),
		field.Time("examined_at").Default(time.Now),
		field.Time("completed_at").Optional().Nillable(),
	}
}

func (ExaminationRecord) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "visit_id"),
		index.Fields("tenant_id", "status"),
	}
}
