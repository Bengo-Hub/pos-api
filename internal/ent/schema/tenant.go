package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Tenant holds the schema definition for the Tenant entity.
type Tenant struct {
	ent.Schema
}

// Fields of the Tenant.
func (Tenant) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty(),
		field.String("slug").
			NotEmpty().
			Unique(),
		field.String("status").
			Default("active"),
		field.String("contact_email").
			Optional().
			Nillable(),
		field.String("contact_phone").
			Optional().
			Nillable(),
		field.String("logo_url").
			Optional().
			Nillable(),
		field.String("website").
			Optional().
			Nillable(),
		field.String("country").
			Optional().
			Nillable().
			Default("KE"),
		field.String("timezone").
			Optional().
			Nillable().
			Default("Africa/Nairobi"),
		field.JSON("brand_colors", map[string]any{}).
			Optional(),
		field.String("org_size").
			Optional().
			Nillable(),
		field.String("use_case").
			Optional().
			Nillable(),
		field.String("subscription_plan").
			Optional().
			Nillable(),
		field.String("subscription_status").
			Optional().
			Nillable(),
		field.Time("subscription_expires_at").
			Optional().
			Nillable(),
		field.String("subscription_id").
			Optional().
			Nillable(),
		field.JSON("tier_limits", map[string]any{}).
			Optional(),
		field.JSON("metadata", map[string]any{}).
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the Tenant.
func (Tenant) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("users", User.Type),
		edge.To("outlets", Outlet.Type),
	}
}

// Indexes of the Tenant.
func (Tenant) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("slug").Unique(),
		index.Fields("status"),
	}
}
