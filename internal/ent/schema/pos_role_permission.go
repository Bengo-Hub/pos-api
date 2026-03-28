package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// POSRolePermission holds the schema definition for the role-permission junction table.
type POSRolePermission struct {
	ent.Schema
}

// Fields of the POSRolePermission.
func (POSRolePermission) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("role_id", uuid.UUID{}).
			Comment("Role identifier"),
		field.UUID("permission_id", uuid.UUID{}).
			Comment("Permission identifier"),
	}
}

// Edges of the POSRolePermission.
func (POSRolePermission) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("role", POSRoleV2.Type).
			Field("role_id").
			Required().
			Unique(),
		edge.To("permission", POSPermission.Type).
			Field("permission_id").
			Required().
			Unique(),
	}
}

// Indexes of the POSRolePermission.
func (POSRolePermission) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("role_id", "permission_id").Unique(),
		index.Fields("role_id"),
		index.Fields("permission_id"),
	}
}
