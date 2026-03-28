package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Table holds the schema definition for the Table entity.
type Table struct {
	ent.Schema
}

// Fields of the Table.
func (Table) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("section_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("FK to sections for floor plan grouping"),
		field.String("name").
			NotEmpty(),
		field.Int("capacity").
			Default(1),
		field.String("status").
			Default("available").
			Comment("available, occupied, reserved, out_of_service"),
		field.Enum("table_type").
			Values("standard", "booth", "bar_seat", "counter", "vip", "vvip").
			Default("standard").
			Comment("Table type for display and pricing rules"),
		field.Float("x_position").
			Optional().
			Nillable().
			Comment("X coordinate for floor plan rendering"),
		field.Float("y_position").
			Optional().
			Nillable().
			Comment("Y coordinate for floor plan rendering"),
		field.JSON("tags", []string{}).
			Optional().
			Comment("Custom labels: VIP, Window, Balcony, etc."),
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

// Edges of the Table.
func (Table) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("section", Section.Type).
			Ref("tables").
			Field("section_id").
			Unique(),
		edge.To("assignments", TableAssignment.Type),
	}
}

// Indexes of the Table.
func (Table) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id", "name").Unique(),
	}
}
