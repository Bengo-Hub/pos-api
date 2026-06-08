package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// RepairJobPart holds the schema definition for a part consumed on a repair job.
type RepairJobPart struct {
	ent.Schema
}

// Fields of the RepairJobPart.
func (RepairJobPart) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("repair_job_id", uuid.UUID{}),
		field.String("inventory_sku").Optional(),
		field.UUID("inventory_item_id", uuid.UUID{}).Optional().Nillable(),
		field.String("description").Optional(),
		field.Float("quantity").Default(1),
		field.Float("unit_cost").GoType(decimal.Decimal{}),
		field.Float("line_total").GoType(decimal.Decimal{}),
	}
}

// Edges of the RepairJobPart.
func (RepairJobPart) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("repair_job", RepairJob.Type).
			Ref("parts").
			Field("repair_job_id").
			Unique().
			Required(),
	}
}

// Indexes of the RepairJobPart.
func (RepairJobPart) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("repair_job_id"),
	}
}
