package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// RateLimitConfig holds rate limiting configuration loaded from the database.
type RateLimitConfig struct {
	ent.Schema
}

// Fields of the RateLimitConfig.
func (RateLimitConfig) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("service_name").
			NotEmpty().
			Comment("Service identifier, e.g. pos-api"),
		field.String("key_type").
			NotEmpty().
			Comment("Rate limit key type: ip, tenant, user, endpoint, global"),
		field.String("endpoint_pattern").
			Default("*").
			Comment("Endpoint pattern to match, e.g. /api/v1/*/pos/orders, * for default"),
		field.Int("requests_per_window").
			Default(60).
			Positive().
			Comment("Maximum requests allowed per window"),
		field.Int("window_seconds").
			Default(60).
			Positive().
			Comment("Time window in seconds"),
		field.Float("burst_multiplier").
			Default(1.5).
			Comment("Burst multiplier for short spikes"),
		field.Bool("is_active").
			Default(true),
		field.String("description").
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the RateLimitConfig.
func (RateLimitConfig) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("service_name", "key_type", "endpoint_pattern").Unique(),
		index.Fields("service_name"),
		index.Fields("is_active"),
	}
}
