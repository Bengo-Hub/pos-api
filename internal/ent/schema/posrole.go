package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// POSRole holds the schema definition for the POSRole entity.
type POSRole struct {
	ent.Schema
}

// Fields of the POSRole.
func (POSRole) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("name").
			NotEmpty(),
		field.String("description").
			Optional(),
		field.JSON("permissions_json", []string{}).
			Default([]string{}),
		field.Bool("is_system").
			Default(false),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the POSRole.
func (POSRole) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("users", UserPOSRole.Type),
	}
}

// Indexes of the POSRole.
func (POSRole) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "name").Unique(),
	}
}
