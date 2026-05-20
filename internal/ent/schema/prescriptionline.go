package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// PrescriptionLine holds the schema definition for the PrescriptionLine entity.
type PrescriptionLine struct{ ent.Schema }

func (PrescriptionLine) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("prescription_id", uuid.UUID{}),
		field.UUID("catalog_item_id", uuid.UUID{}).Optional().Nillable(),
		field.String("drug_name").NotEmpty(),
		field.String("dosage").Optional(),
		field.String("form").Optional(),
		field.String("instructions").Optional(),
		field.Int("quantity_prescribed"),
		field.Int("quantity_dispensed").Default(0),
		field.Float("unit_price").GoType(decimal.Decimal{}).Optional().Nillable(),
		field.String("lot_number").Optional(),
		field.Time("expiry_date").Optional().Nillable(),
		field.String("status").Default("pending"),
	}
}

func (PrescriptionLine) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("prescription_id"),
		index.Fields("catalog_item_id"),
	}
}
