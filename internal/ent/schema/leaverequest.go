package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// LeaveRequest tracks staff leave applications with a manager-approval workflow.
type LeaveRequest struct {
	ent.Schema
}

func (LeaveRequest) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("staff_member_id", uuid.UUID{}),
		field.Time("start_date"),
		field.Time("end_date"),
		field.Enum("leave_type").
			Values("annual", "sick", "unpaid", "maternity", "compassionate", "other"),
		field.String("reason").Optional().Nillable(),
		field.Enum("status").
			Values("pending", "approved", "rejected").
			Default("pending"),
		field.UUID("requested_by", uuid.UUID{}),
		field.UUID("approved_by", uuid.UUID{}).Optional().Nillable(),
		field.String("rejection_reason").Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (LeaveRequest) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "staff_member_id"),
		index.Fields("tenant_id"),
		index.Fields("status"),
	}
}
