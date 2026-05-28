package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// StaffOutlet is the join table linking a StaffMember to the outlets they are assigned to.
// A staff member can be assigned to multiple outlets (many-to-many).
type StaffOutlet struct {
	ent.Schema
}

// Fields of the StaffOutlet.
func (StaffOutlet) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("staff_member_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.Bool("is_home_outlet").Default(false).
			Comment("True for the staff member's primary outlet"),
		field.Time("assigned_at").Default(time.Now).Immutable(),
	}
}

// Edges of the StaffOutlet.
func (StaffOutlet) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("staff_member", StaffMember.Type).
			Ref("outlets").
			Field("staff_member_id").
			Unique().
			Required(),
		edge.From("outlet", Outlet.Type).
			Ref("staff_outlets").
			Field("outlet_id").
			Unique().
			Required(),
	}
}

// Indexes of the StaffOutlet.
func (StaffOutlet) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("staff_member_id", "outlet_id").Unique(),
		index.Fields("tenant_id", "outlet_id"),
	}
}
