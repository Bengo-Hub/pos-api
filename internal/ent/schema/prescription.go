package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Prescription holds the schema definition for the Prescription entity.
type Prescription struct{ ent.Schema }

func (Prescription) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("order_id", uuid.UUID{}).Optional().Nillable(),
		field.String("prescription_number").NotEmpty(),
		field.String("prescriber_name").Optional(),
		field.String("prescriber_license").Optional(),
		field.String("patient_name").NotEmpty(),
		field.String("patient_dob").Optional(),
		field.String("patient_id_number").Optional(),
		field.String("status").Default("pending"),
		field.String("notes").Optional(),
		field.Time("dispensed_at").Optional().Nillable(),
		field.UUID("dispensed_by", uuid.UUID{}).Optional().Nillable(),
		// Single JSON bucket for prescription-workflow fields that are only ever read/written
		// against a single already-looked-up prescription (never filtered/joined across rows) —
		// keeps additive pharmacy-workflow evolution (interaction-check ref, approval audit,
		// stock-reservation ref, optional CRM link) to one migration instead of one per field.
		// Known keys: allergy_flags ([]string), interaction_check_id (uuid string),
		// approved_by (uuid string), approved_at (RFC3339 string), reservation_id (uuid string),
		// crm_contact_id (uuid string).
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (Prescription) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id"),
		index.Fields("outlet_id"),
		index.Fields("prescription_number"),
		index.Fields("status"),
		index.Fields("order_id"),
	}
}
