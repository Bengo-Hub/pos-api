package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// User holds the schema definition for the User entity.
type User struct {
	ent.Schema
}

// Fields of the User.
func (User) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("auth_service_user_id", uuid.UUID{}).
			Optional().
			Unique().
			Comment("Reference to auth-service user. Identity data synced from auth-service."),
		field.String("email").
			NotEmpty(),
		field.String("sync_status").
			Default("pending").
			Comment("Sync status with auth-service: pending, synced, failed"),
		field.Time("sync_at").
			Optional().
			Comment("Last sync timestamp with auth-service"),
		field.String("full_name").
			NotEmpty(),
		field.String("phone").
			Optional(),
		field.String("status").
			Default("active"),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the User.
func (User) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("users").
			Field("tenant_id").
			Required().
			Unique(),
		edge.To("pos_roles", UserPOSRole.Type),
		edge.To("pos_sessions", POSDeviceSession.Type),
	}
}

// Indexes of the User.
func (User) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "email").Unique(),
		index.Fields("tenant_id", "auth_service_user_id"),
		index.Fields("email"),
		index.Fields("sync_status"),
	}
}
