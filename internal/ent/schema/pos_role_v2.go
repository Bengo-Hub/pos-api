package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// POSRoleV2 holds the schema definition for the new RBAC roles
// (coexists with existing POSRole for backward compatibility).
type POSRoleV2 struct {
	ent.Schema
}

// Fields of the POSRoleV2.
func (POSRoleV2) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Tenant identifier; NULL = global/system role shared platform-wide across all tenants. Non-NULL = tenant-specific custom role."),
		field.String("role_code").
			NotEmpty().
			Comment("Role code: admin, manager, cashier, waiter, kitchen, bar, receptionist, viewer"),
		field.String("name").
			NotEmpty().
			Comment("Display name"),
		field.Text("description").
			Optional(),
		field.Bool("is_system_role").
			Default(false).
			Comment("System roles cannot be deleted"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the POSRoleV2.
func (POSRoleV2) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("permissions", POSPermission.Type).Through("role_permissions", POSRolePermission.Type),
		edge.To("user_assignments", POSUserRoleAssignment.Type),
	}
}

// Indexes of the POSRoleV2.
func (POSRoleV2) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id"),
		// Unique per tenant for custom roles. Global system roles (tenant_id NULL) are kept
		// unique by code via deterministic seed IDs + existence checks (same approach as ServiceConfig).
		index.Fields("tenant_id", "role_code").Unique(),
		// Fast lookup of a role by code regardless of tenant (global resolution path).
		index.Fields("role_code"),
		index.Fields("is_system_role"),
	}
}
