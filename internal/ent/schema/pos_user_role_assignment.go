package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// POSUserRoleAssignment holds the schema definition for user role assignments.
type POSUserRoleAssignment struct {
	ent.Schema
}

// Fields of the POSUserRoleAssignment.
func (POSUserRoleAssignment) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Tenant identifier"),
		field.UUID("user_id", uuid.UUID{}).
			Comment("User identifier (pos user)"),
		field.UUID("role_id", uuid.UUID{}).
			Comment("Role identifier"),
		field.UUID("assigned_by", uuid.UUID{}).
			Comment("User who assigned the role"),
		field.Time("assigned_at").
			Default(time.Now).
			Immutable(),
		field.Time("expires_at").
			Optional().
			Nillable().
			Comment("Optional expiration date"),
	}
}

// Edges of the POSUserRoleAssignment.
func (POSUserRoleAssignment) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("user", User.Type).
			Field("user_id").
			Required().
			Unique(),
		edge.To("role", POSRoleV2.Type).
			Field("role_id").
			Required().
			Unique(),
	}
}

// Indexes of the POSUserRoleAssignment.
func (POSUserRoleAssignment) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "user_id", "role_id").Unique(),
		index.Fields("tenant_id"),
		index.Fields("user_id"),
		index.Fields("role_id"),
		index.Fields("expires_at"),
	}
}
