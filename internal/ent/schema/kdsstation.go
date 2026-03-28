package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// KDSStation holds the schema definition for the KDSStation entity.
type KDSStation struct {
	ent.Schema
}

// Fields of the KDSStation.
func (KDSStation) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.String("name").NotEmpty().Comment("Station name: Grill, Fryer, Bar, Cold Station"),
		field.JSON("category_filter", []string{}).Optional().Comment("Category codes this station handles"),
		field.Int("sort_order").Default(0),
		field.Bool("is_active").Default(true),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

// Edges of the KDSStation.
func (KDSStation) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("tickets", KDSTicket.Type),
	}
}

// Indexes of the KDSStation.
func (KDSStation) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id"),
	}
}
