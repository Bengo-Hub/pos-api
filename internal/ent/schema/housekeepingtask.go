package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// HousekeepingTask tracks room cleaning, turndown, and maintenance tasks.
// Created automatically on checkout; can also be created manually.
type HousekeepingTask struct {
	ent.Schema
}

func (HousekeepingTask) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("room_id", uuid.UUID{}),
		field.UUID("room_guest_id", uuid.UUID{}).Optional().Nillable().
			Comment("Set for post-checkout tasks; nil for routine cleans"),
		field.Enum("task_type").
			Values("checkout_clean", "routine_clean", "turndown", "maintenance", "inspection").
			Default("routine_clean"),
		field.Enum("status").
			Values("pending", "in_progress", "completed", "cancelled").
			Default("pending"),
		field.Enum("priority").
			Values("normal", "urgent").
			Default("normal"),
		field.UUID("assigned_to", uuid.UUID{}).Optional().Nillable().
			Comment("staff_member_id from pos-api StaffMember"),
		field.String("notes").Optional(),
		field.Time("due_at").Optional().Nillable(),
		field.Time("completed_at").Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (HousekeepingTask) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("room", Room.Type).Ref("housekeeping_tasks").Field("room_id").Unique().Required(),
	}
}

func (HousekeepingTask) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "room_id"),
		index.Fields("tenant_id", "status"),
		index.Fields("tenant_id", "assigned_to"),
	}
}
