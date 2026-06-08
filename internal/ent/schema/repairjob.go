package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// RepairJob holds the schema definition for the RepairJob (job-card) entity.
// A repair job tracks a device brought in for repair through its lifecycle:
// intake → diagnosed → awaiting_parts → in_progress → ready → completed (or cancelled).
type RepairJob struct {
	ent.Schema
}

// Fields of the RepairJob.
func (RepairJob) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}).Optional().Nillable(),
		field.String("job_number").NotEmpty().Comment("Auto-generated human-readable job-card number"),
		field.String("customer_phone").Optional(),
		field.String("customer_name").Optional(),
		field.String("device_description").Optional().Comment("e.g. 'iPhone 13 Pro, Space Gray, IMEI ...'"),
		field.Text("reported_issue").Optional().Comment("Customer-reported fault"),
		field.Enum("status").
			Values("intake", "diagnosed", "awaiting_parts", "in_progress", "ready", "completed", "cancelled").
			Default("intake"),
		field.String("diagnosis").Optional().Comment("Technician diagnosis"),
		field.Float("estimated_cost").GoType(decimal.Decimal{}).Comment("Initial estimate at intake"),
		field.Float("quoted_cost").GoType(decimal.Decimal{}).Optional().Nillable().Comment("Final quoted cost after diagnosis"),
		field.UUID("assigned_staff_id", uuid.UUID{}).Optional().Nillable().Comment("Assigned technician"),
		field.UUID("pos_order_id", uuid.UUID{}).Optional().Nillable().Comment("Linked POS order set when settled via POS"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

// Edges of the RepairJob.
func (RepairJob) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("parts", RepairJobPart.Type),
		edge.To("events", RepairJobEvent.Type),
	}
}

// Indexes of the RepairJob.
func (RepairJob) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "status"),
		index.Fields("tenant_id", "job_number").Unique(),
	}
}
