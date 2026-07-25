package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Patient is the OPD/clinic patient registration record — the real entity the Records module
// creates, which Triage/Examination/Lab/Pharmacy all reference by patient_id. Distinct from the
// existing standalone-pharmacy Prescription.patient_name/dob/id_number free-text fields, which
// remain for a walk-in/external-prescription sale where no formal registration ever happens.
type Patient struct{ ent.Schema }

func (Patient) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}).Comment("Registering outlet"),
		field.String("patient_number").NotEmpty().Comment("Sequence-generated (Settings -> Documents)"),
		field.String("full_name").NotEmpty(),
		field.Time("dob").Optional().Nillable(),
		field.String("gender").Optional(),
		field.String("phone").Optional(),
		field.String("id_number").Optional().Comment("National ID / passport"),
		field.String("address").Optional(),
		field.JSON("allergy_flags", []string{}).Optional().Default([]string{}).
			Comment("Patient-level allergy flags, checked at examination/prescribing time"),
		// Pointer only — marketflow/CRM remains the source of truth for contact identity, no PII
		// duplication (same pattern as Prescription.metadata.crm_contact_id).
		field.UUID("crm_contact_id", uuid.UUID{}).Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (Patient) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "patient_number").Unique(),
		index.Fields("tenant_id", "phone"),
		index.Fields("tenant_id", "id_number"),
		index.Fields("tenant_id", "full_name"),
	}
}
