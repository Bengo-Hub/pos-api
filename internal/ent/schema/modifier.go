package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// Modifier holds the schema definition for the Modifier entity.
type Modifier struct {
	ent.Schema
}

// Fields of the Modifier.
func (Modifier) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("modifier_group_id", uuid.UUID{}),
		field.String("name").
			NotEmpty(),
		field.Float("price_override").
			Optional().
			Nillable(),
		field.Bool("is_available").
			Default(true),
		field.UUID("inventory_modifier_option_id", uuid.UUID{}).Optional().Nillable().Comment("FK to inventory master modifier option for sync"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the Modifier.
func (Modifier) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("group", ModifierGroup.Type).
			Ref("modifiers").
			Field("modifier_group_id").
			Unique().
			Required(),
	}
}
