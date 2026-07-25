package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// TriageRecord captures the vitals taken for a PatientVisit before examination. A visit may be
// re-triaged (vitals recheck) — rows are append-only, the latest by taken_at is authoritative.
type TriageRecord struct{ ent.Schema }

func (TriageRecord) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("visit_id", uuid.UUID{}),
		field.UUID("taken_by", uuid.UUID{}).Comment("Staff member who took vitals"),
		field.Int("bp_systolic").Optional().Nillable(),
		field.Int("bp_diastolic").Optional().Nillable(),
		field.Float("temperature_celsius").Optional().Nillable(),
		field.Int("pulse_bpm").Optional().Nillable(),
		field.Int("respiration_rate").Optional().Nillable(),
		field.Float("spo2_percent").Optional().Nillable(),
		field.Float("weight_kg").Optional().Nillable(),
		field.Float("height_cm").Optional().Nillable(),
		field.String("notes").Optional(),
		field.Time("taken_at").Default(time.Now).Immutable(),
	}
}

func (TriageRecord) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "visit_id"),
	}
}
