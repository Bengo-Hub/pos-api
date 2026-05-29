package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// StaffShiftOverride captures a one-off date-specific exception to the recurring weekly schedule.
// override_type=off_duty: not working this date.
// override_type=manual_shift: custom hours replace the regular schedule.
// override_type=half_day: partial hours (start_time + end_time required).
type StaffShiftOverride struct {
	ent.Schema
}

func (StaffShiftOverride) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("staff_member_id", uuid.UUID{}),
		field.Time("date").Comment("Calendar date this override applies to"),
		field.Enum("override_type").
			Values("off_duty", "manual_shift", "half_day"),
		field.String("start_time").Optional().Nillable().Comment("HH:MM — required for manual_shift / half_day"),
		field.String("end_time").Optional().Nillable().Comment("HH:MM — required for manual_shift / half_day"),
		field.String("reason").Optional().Nillable(),
		field.Enum("status").
			Values("pending", "approved", "rejected").
			Default("approved").
			Comment("Manager-created overrides are auto-approved; staff self-submissions start as pending"),
		field.UUID("created_by", uuid.UUID{}),
		field.UUID("approved_by", uuid.UUID{}).Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (StaffShiftOverride) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "staff_member_id", "date").Unique(),
		index.Fields("tenant_id"),
		index.Fields("staff_member_id"),
		index.Fields("date"),
	}
}
