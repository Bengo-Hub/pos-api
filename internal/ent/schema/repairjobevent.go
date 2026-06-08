package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// RepairJobEvent holds the schema definition for an audit/timeline event on a repair job.
type RepairJobEvent struct {
	ent.Schema
}

// Fields of the RepairJobEvent.
func (RepairJobEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("repair_job_id", uuid.UUID{}),
		field.String("event_type").
			NotEmpty().
			Comment("intake, diagnosis, parts_added, status_change, note, settled"),
		field.Text("notes").Optional(),
		field.UUID("actor_id", uuid.UUID{}).Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

// Edges of the RepairJobEvent.
func (RepairJobEvent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("repair_job", RepairJob.Type).
			Ref("events").
			Field("repair_job_id").
			Unique().
			Required(),
	}
}

// Indexes of the RepairJobEvent.
func (RepairJobEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("repair_job_id"),
	}
}
