package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// ChannelIntegration holds the schema definition for the ChannelIntegration entity.
type ChannelIntegration struct {
	ent.Schema
}

// Fields of the ChannelIntegration.
func (ChannelIntegration) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("channel_name").
			NotEmpty(),
		field.String("channel_type").
			NotEmpty().
			Comment("uber_eats | glovo | etc"),
		field.JSON("config_json", map[string]any{}).
			Default(map[string]any{}),
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
