package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Outlet holds the schema definition for the Outlet entity.
type Outlet struct {
	ent.Schema
}

// Fields of the Outlet.
func (Outlet) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("tenant_slug").
			NotEmpty(),
		field.String("code").
			NotEmpty(),
		field.String("name").
			NotEmpty(),
		field.String("channel_type").
			Default("physical"),
		field.JSON("address_json", map[string]any{}).
			Optional(),
		field.String("timezone").
			Default("Africa/Nairobi"),
		field.String("status").
			Default("active"),
		field.String("use_case").
			Optional().
			Nillable().
			Comment("Use case for this outlet (e.g., hospitality, retail)"),
		field.Time("opened_at").
			Optional().
			Nillable(),
		field.Time("closed_at").
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the Outlet.
func (Outlet) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tenant", Tenant.Type).
			Ref("outlets").
			Field("tenant_id").
			Unique().
			Required(),
		edge.To("settings", OutletSetting.Type).Unique(),
		edge.To("devices", POSDevice.Type),
	}
}

// Indexes of the Outlet.
func (Outlet) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "code").Unique(),
		index.Fields("tenant_slug"),
	}
}
