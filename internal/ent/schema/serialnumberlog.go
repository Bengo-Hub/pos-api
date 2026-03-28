package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// SerialNumberLog holds the schema definition for the SerialNumberLog entity.
type SerialNumberLog struct {
	ent.Schema
}

// Fields of the SerialNumberLog.
func (SerialNumberLog) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("order_line_id", uuid.UUID{}).Comment("FK to POSOrderLine"),
		field.String("serial_number").NotEmpty(),
		field.String("item_sku").NotEmpty(),
		field.Time("sold_at").Default(time.Now),
	}
}

// Indexes of the SerialNumberLog.
func (SerialNumberLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "serial_number"),
		index.Fields("tenant_id", "item_sku"),
		index.Fields("order_line_id"),
	}
}
