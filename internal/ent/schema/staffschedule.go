package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// StaffSchedule holds the recurring weekly availability for a staff member.
type StaffSchedule struct {
	ent.Schema
}

func (StaffSchedule) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("staff_member_id", uuid.UUID{}),
		field.Int("day_of_week").Comment("0=Sunday … 6=Saturday"),
		field.String("start_time").Comment("HH:MM e.g. 08:00"),
		field.String("end_time").Comment("HH:MM e.g. 17:00"),
		field.Bool("is_available").Default(true),
		field.String("notes").Optional(),
	}
}

func (StaffSchedule) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "staff_member_id", "day_of_week").Unique(),
		index.Fields("staff_member_id"),
		index.Fields("outlet_id"),
	}
}
