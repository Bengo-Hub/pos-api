package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ServiceConfig holds service-level configuration key-value pairs.
// When tenant_id is nil, it represents a platform-level default.
// Tenant-specific overrides have a non-nil tenant_id.
type ServiceConfig struct {
	ent.Schema
}

// Fields of the ServiceConfig.
func (ServiceConfig) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Nil = platform-level default; set = tenant-specific override"),
		field.String("config_key").
			NotEmpty().
			Comment("Configuration key, e.g. pos.max_order_amount"),
		field.Text("config_value").
			NotEmpty().
			Comment("Configuration value as JSON string"),
		field.String("config_type").
			Default("string").
			Comment("Value type: string, int, bool, json, float"),
		field.String("description").
			Optional(),
		field.Bool("is_secret").
			Default(false).
			Comment("If true, value is masked in API responses"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the ServiceConfig.
func (ServiceConfig) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "config_key").Unique(),
		index.Fields("config_key"),
	}
}
