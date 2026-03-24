package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// StaffMember holds the schema definition for the StaffMember entity.
type StaffMember struct {
	ent.Schema
}

// Fields of the StaffMember.
func (StaffMember) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("user_id", uuid.UUID{}).Comment("Auth-service user ID"),
		field.String("name").NotEmpty(),
		field.JSON("service_skus", []string{}).Optional().Comment("Service SKUs this staff can perform"),
		field.JSON("working_hours", map[string]any{}).Optional().Comment("Weekly schedule"),
		field.Float("commission_rate").Optional().Nillable().Comment("Commission percentage on services"),
		field.Bool("is_active").Default(true),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

// Indexes of the StaffMember.
func (StaffMember) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id"),
		index.Fields("tenant_id", "user_id").Unique(),
	}
}
