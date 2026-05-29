package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ShiftRotation defines a recurring multi-week rotation pattern for a group of staff.
// Example: a two-week rotation where Alice works Mon-Fri in week A and Tue-Sat in week B.
type ShiftRotation struct {
	ent.Schema
}

func (ShiftRotation) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}).Optional().Nillable(),
		field.String("name").NotEmpty(),
		field.Int("cycle_days").Default(14).
			Comment("Rotation cycle length in days (e.g. 14 = two-week rotation)"),
		field.Time("start_date").
			Comment("Calendar date the rotation cycle begins from"),
		field.Bool("is_active").Default(true),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (ShiftRotation) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id"),
		index.Fields("tenant_id", "is_active"),
	}
}

// ShiftRotationSlot defines a single staff member's shift for one day within the rotation cycle.
// cycle_day is 1-based and ranges from 1..cycle_days.
type ShiftRotationSlot struct {
	ent.Schema
}

func (ShiftRotationSlot) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("rotation_id", uuid.UUID{}),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("staff_member_id", uuid.UUID{}),
		field.Int("cycle_day").
			Comment("1-based position in the cycle (1 = first day of cycle, cycle_days = last day)"),
		field.String("start_time").Comment("HH:MM"),
		field.String("end_time").Comment("HH:MM"),
		field.Bool("is_off_day").Default(false).
			Comment("Staff is off on this cycle day regardless of start/end times"),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (ShiftRotationSlot) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("rotation_id"),
		index.Fields("rotation_id", "staff_member_id", "cycle_day").Unique(),
		index.Fields("tenant_id"),
	}
}
