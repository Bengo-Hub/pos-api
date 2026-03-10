package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// OrderLink holds the schema definition for the OrderLink entity.
type OrderLink struct {
	ent.Schema
}

// Fields of the OrderLink.
func (OrderLink) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("order_id", uuid.UUID{}),
		field.String("external_order_id").
			NotEmpty(),
		field.String("channel_source").
			NotEmpty(),
		field.Time("linked_at").
			Default(time.Now),
	}
}
