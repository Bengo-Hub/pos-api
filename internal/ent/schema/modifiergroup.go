package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// ModifierGroup holds the schema definition for the ModifierGroup entity.
type ModifierGroup struct {
	ent.Schema
}

// Fields of the ModifierGroup.
func (ModifierGroup) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("name").
			NotEmpty(),
		field.Int("min_selection").
			Default(0),
		field.Int("max_selection").
			Default(1),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the ModifierGroup.
func (ModifierGroup) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("modifiers", Modifier.Type),
	}
}
