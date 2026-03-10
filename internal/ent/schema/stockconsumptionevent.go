package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// StockConsumptionEvent holds the schema definition for the StockConsumptionEvent entity.
type StockConsumptionEvent struct {
	ent.Schema
}

// Fields of the StockConsumptionEvent.
func (StockConsumptionEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("catalog_item_id", uuid.UUID{}),
		field.Float("quantity_consumed"),
		field.UUID("order_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Time("occurred_at").
			Default(time.Now),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
	}
}
