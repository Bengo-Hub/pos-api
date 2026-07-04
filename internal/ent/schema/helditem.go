package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// HeldItem is an already-prepared item set aside instead of being voided — e.g. the waiter opened
// a Mango Juice by mistake but the customer wanted Orange Juice. Rather than voiding (and wasting)
// the made item, it's held in a per-outlet pool until another customer claims (buys) it. If it's
// still unclaimed when the waiter closes their shift, they must void it (the shift-close guard).
type HeldItem struct {
	ent.Schema
}

// Fields of the HeldItem.
func (HeldItem) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("source_order_id", uuid.UUID{}).
			Comment("The order the item was originally added to (and removed from when set aside)."),
		field.UUID("source_line_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("The POSOrderLine that was set aside, if known."),
		field.String("catalog_item_id").Optional(),
		field.String("sku").Optional(),
		field.String("name").NotEmpty().Comment("Item name for the held-items list."),
		field.Float("quantity").Default(1),
		field.Float("unit_price").Default(0),
		field.String("reason").Optional().Comment("Why it was set aside (e.g. wrong order)."),
		field.String("status").
			Default("held").
			Comment("held | claimed | voided"),
		field.UUID("held_by_user_id", uuid.UUID{}).Comment("Waiter/cashier who set it aside."),
		field.UUID("shift_session_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("The device shift/session that must clear it before closing."),
		field.UUID("claimed_order_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("The order that claimed (bought) this held item."),
		field.UUID("resolved_by_user_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Who claimed or voided it."),
		field.Time("resolved_at").Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

// Indexes of the HeldItem.
func (HeldItem) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id", "status"),
		index.Fields("tenant_id", "shift_session_id", "status"),
	}
}
