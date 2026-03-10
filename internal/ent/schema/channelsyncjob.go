package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// ChannelSyncJob holds the schema definition for the ChannelSyncJob entity.
type ChannelSyncJob struct {
	ent.Schema
}

// Fields of the ChannelSyncJob.
func (ChannelSyncJob) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("integration_id", uuid.UUID{}),
		field.String("job_type").
			NotEmpty().
			Comment("catalog_sync | order_poll"),
		field.String("status").
			Default("pending"),
		field.JSON("result_json", map[string]any{}).
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("finished_at").
			Optional().
			Nillable(),
	}
}
