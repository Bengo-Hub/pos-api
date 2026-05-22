package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// PosNotification holds in-app notifications for POS staff (e.g. waiter order-ready alerts).
type PosNotification struct {
	ent.Schema
}

func (PosNotification) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("user_id", uuid.UUID{}).Comment("Recipient staff member"),
		field.String("notification_type").NotEmpty().Comment("e.g. kds.order_ready"),
		field.String("title").NotEmpty(),
		field.String("body").Default(""),
		field.JSON("payload", map[string]any{}).Default(map[string]any{}),
		field.Bool("is_read").Default(false),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (PosNotification) Edges() []ent.Edge {
	return nil
}

func (PosNotification) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "user_id", "is_read"),
		index.Fields("tenant_id", "outlet_id"),
	}
}
