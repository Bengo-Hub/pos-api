package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// WebhookSubscription holds the schema definition for the WebhookSubscription entity.
type WebhookSubscription struct {
	ent.Schema
}

// Fields of the WebhookSubscription.
func (WebhookSubscription) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("url").
			NotEmpty(),
		field.String("event_type").
			NotEmpty(),
		field.String("secret_key").
			NotEmpty(),
		field.String("status").
			Default("active"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}
