package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// StockAlertSubscription holds the schema definition for the StockAlertSubscription entity.
type StockAlertSubscription struct {
	ent.Schema
}

// Fields of the StockAlertSubscription.
func (StockAlertSubscription) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("catalog_item_id", uuid.UUID{}),
		field.Float("threshold_low"),
		field.Float("threshold_critical"),
		field.String("notification_channel").
			Default("email"),
		field.String("status").
			Default("active"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}
