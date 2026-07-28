package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// LabTest is the tenant's configurable catalogue of orderable lab tests, each with its own price.
// The Examination stage picks from this list (filtered by category) instead of free-typing a test
// name, and the price drives the pre-payment bill a patient must settle before the Lab module
// activates the order.
type LabTest struct{ ent.Schema }

func (LabTest) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("name").NotEmpty(),
		field.String("code").Optional().Comment("Lab/LOINC-style short code, e.g. FBC"),
		field.String("category").Optional().Comment("Haematology | Biochemistry | Microbiology | Serology | Imaging | …"),
		field.Float("price").GoType(decimal.Decimal{}),
		field.String("sample_type").Optional().Comment("Blood | Urine | Stool | Swab | …"),
		field.String("reference_range").Optional().Comment("Default reference range pre-filled on the result line"),
		field.String("unit").Optional(),
		field.Int("turnaround_hours").Optional().Nillable(),
		field.Bool("is_active").Default(true),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (LabTest) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "is_active"),
		index.Fields("tenant_id", "category"),
		index.Fields("tenant_id", "name").Unique(),
	}
}
