package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// TenantSyncEvent holds the schema definition for the TenantSyncEvent entity.
type TenantSyncEvent struct {
	ent.Schema
}

// Fields of the TenantSyncEvent.
func (TenantSyncEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("event_type").
			NotEmpty(),
		field.JSON("payload", map[string]any{}).
			Optional(),
		field.Time("occurred_at").
			Default(time.Now),
	}
}
