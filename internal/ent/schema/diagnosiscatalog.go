package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// DiagnosisCatalog is the pick-list the Examination stage offers for diagnoses. tenant_id ==
// uuid.Nil marks a PLATFORM-GLOBAL curated entry (seeded common conditions, shared by every
// tenant); a real tenant_id marks one that tenant added themselves. The clinician can always type
// a diagnosis that isn't in the list — it's saved as a tenant-scoped row so the catalogue grows
// organically rather than forcing an admin to pre-configure everything. Mirrors the tenant-override
// -over-global pattern DrugInteractionRule already uses.
type DiagnosisCatalog struct{ ent.Schema }

func (DiagnosisCatalog) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).Comment("uuid.Nil = platform-global curated entry"),
		field.Bool("is_global").Default(false),
		field.String("name").NotEmpty(),
		field.String("code").Optional().Comment("ICD-10/11 code where known, e.g. J06.9"),
		field.String("category").Optional().Comment("Respiratory | Gastrointestinal | Infectious | …"),
		field.Bool("is_active").Default(true),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (DiagnosisCatalog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "name").Unique(),
		index.Fields("tenant_id", "category"),
		index.Fields("is_global"),
	}
}
