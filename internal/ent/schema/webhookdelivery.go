package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type WebhookDelivery struct{ ent.Schema }

func (WebhookDelivery) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("subscription_id", uuid.UUID{}),
		field.String("event_type"),
		field.Text("payload"),
		field.Int("http_status").Optional().Nillable(),
		field.Text("response_body").Optional(),
		field.String("error_message").Optional(),
		field.Int("attempt").Default(1),
		field.String("status").Default("pending").Comment("pending/success/failed"),
		field.Time("delivered_at").Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (WebhookDelivery) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("subscription_id"),
		index.Fields("status"),
	}
}
