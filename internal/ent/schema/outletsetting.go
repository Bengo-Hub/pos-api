package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// OutletSetting holds the schema definition for the OutletSetting entity.
type OutletSetting struct {
	ent.Schema
}

// Fields of the OutletSetting.
func (OutletSetting) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("outlet_id", uuid.UUID{}),
		field.JSON("receipts_json", map[string]any{}).
			Optional(),
		field.JSON("tax_config_json", map[string]any{}).
			Optional(),
		field.JSON("service_charge_json", map[string]any{}).
			Optional(),
		field.JSON("opening_hours_json", map[string]any{}).
			Optional(),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the OutletSetting.
func (OutletSetting) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("outlet", Outlet.Type).
			Ref("settings").
			Field("outlet_id").
			Unique().
			Required(),
	}
}
