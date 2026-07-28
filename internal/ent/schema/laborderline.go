package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// LabOrderLine is one requested test within a LabOrder — a lab tech enters the result directly on
// the line (no external LIS/equipment integration; that's a real future integration point but out
// of scope here).
type LabOrderLine struct{ ent.Schema }

func (LabOrderLine) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("lab_order_id", uuid.UUID{}),
		// Links back to the LabTest catalogue entry this line was picked from (nil for a
		// free-typed one-off test). Carries the price charged at order time so a later catalogue
		// price change never rewrites an already-billed order.
		field.UUID("lab_test_id", uuid.UUID{}).Optional().Nillable(),
		field.Float("price").GoType(decimal.Decimal{}).Optional(),
		field.String("test_name").NotEmpty(),
		field.String("result").Optional(),
		field.String("unit").Optional(),
		field.String("reference_range").Optional(),
		field.Enum("flag").
			Values("pending", "normal", "abnormal", "critical").
			Default("pending"),
		field.String("notes").Optional(),
		field.UUID("resulted_by", uuid.UUID{}).Optional().Nillable(),
		field.Time("resulted_at").Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (LabOrderLine) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("lab_order_id"),
	}
}
