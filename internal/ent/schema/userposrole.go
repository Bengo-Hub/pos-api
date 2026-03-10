package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// UserPOSRole holds the schema definition for the UserPOSRole entity.
type UserPOSRole struct {
	ent.Schema
}

// Fields of the UserPOSRole.
func (UserPOSRole) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("user_id", uuid.UUID{}),
		field.UUID("role_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Edges of the UserPOSRole.
func (UserPOSRole) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("pos_roles").
			Field("user_id").
			Unique().
			Required(),
		edge.From("role", POSRole.Type).
			Ref("users").
			Field("role_id").
			Unique().
			Required(),
	}
}
