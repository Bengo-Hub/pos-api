package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// IntegrationSetting holds the schema definition for the IntegrationSetting entity.
type IntegrationSetting struct {
	ent.Schema
}

// Fields of the IntegrationSetting.
func (IntegrationSetting) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("provider_name").
			NotEmpty(),
		field.String("setting_key").
			NotEmpty(),
		field.String("setting_value").
			Optional(),
		field.JSON("setting_json", map[string]any{}).
			Optional(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the IntegrationSetting.
func (IntegrationSetting) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "provider_name", "setting_key").Unique(),
	}
}
