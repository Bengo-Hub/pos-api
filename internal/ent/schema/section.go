package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Section represents a physical area within an outlet (e.g., Main Hall, Patio, VIP Lounge).
// Sections organize tables for floor plan rendering and service management.
type Section struct {
	ent.Schema
}

// Fields of the Section.
func (Section) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.String("name").
			NotEmpty().
			Comment("Display name (e.g., Main Hall, Outdoor Patio)"),
		field.String("slug").
			NotEmpty().
			Comment("URL-safe identifier"),
		field.Text("description").
			Optional(),
		field.Int("floor_number").
			Default(1).
			Comment("Floor level (1 = ground floor)"),
		field.Int("sort_order").
			Default(0),
		field.Bool("is_active").
			Default(true),
		field.Enum("section_type").
			Values("main_hall", "outdoor", "private_room", "bar", "vip", "vvip", "rooftop", "other").
			Default("main_hall"),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}).
			Comment("Additional config: color, icon, capacity"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the Section.
func (Section) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("tables", Table.Type),
	}
}

// Indexes of the Section.
func (Section) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id"),
		index.Fields("tenant_id", "outlet_id", "slug").Unique(),
		index.Fields("is_active"),
	}
}
