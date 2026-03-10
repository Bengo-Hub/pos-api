package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// SyncFailure holds the schema definition for the SyncFailure entity.
type SyncFailure struct {
	ent.Schema
}

// Fields of the SyncFailure.
func (SyncFailure) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("entity_type").
			NotEmpty(),
		field.String("external_id").
			NotEmpty(),
		field.String("error_message").
			NotEmpty(),
		field.JSON("payload", map[string]any{}).
			Optional(),
		field.Time("occurred_at").
			Default(time.Now),
		field.Bool("is_resolved").
			Default(false),
	}
}
