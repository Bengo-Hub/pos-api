package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// InventorySnapshot holds the schema definition for the InventorySnapshot entity.
type InventorySnapshot struct {
	ent.Schema
}

// Fields of the InventorySnapshot.
func (InventorySnapshot) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("catalog_item_id", uuid.UUID{}),
		field.Float("quantity_on_hand"),
		field.Time("captured_at").
			Default(time.Now),
	}
}
