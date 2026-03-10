package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// TableAssignment holds the schema definition for the TableAssignment entity.
type TableAssignment struct {
	ent.Schema
}

// Fields of the TableAssignment.
func (TableAssignment) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("table_id", uuid.UUID{}),
		field.UUID("order_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("bar_tab_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Time("assigned_at").
			Default(time.Now),
		field.Time("released_at").
			Optional().
			Nillable(),
	}
}

// Edges of the TableAssignment.
func (TableAssignment) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("table", Table.Type).
			Ref("assignments").
			Field("table_id").
			Unique().
			Required(),
	}
}
