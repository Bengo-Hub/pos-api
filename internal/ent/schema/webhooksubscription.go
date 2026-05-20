package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type WebhookSubscription struct{ ent.Schema }

func (WebhookSubscription) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}).Optional().Nillable(),
		field.String("event_type").Comment("e.g. order.completed, payment.received"),
		field.String("target_url"),
		field.String("secret").Optional().Comment("HMAC signing secret"),
		field.Bool("is_active").Default(true),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (WebhookSubscription) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "event_type"),
		index.Fields("tenant_id"),
	}
}
