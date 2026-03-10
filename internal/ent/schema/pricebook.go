package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// PriceBook holds the schema definition for the PriceBook entity.
type PriceBook struct {
	ent.Schema
}

// Fields of the PriceBook.
func (PriceBook) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("name").
			NotEmpty(),
		field.String("description").
			Optional(),
		field.Bool("is_default").
			Default(false),
		field.Time("start_at").
			Optional().
			Nillable(),
		field.Time("end_at").
			Optional().
			Nillable(),
		field.String("status").
			Default("active"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the PriceBook.
func (PriceBook) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("items", PriceBookItem.Type),
	}
}
