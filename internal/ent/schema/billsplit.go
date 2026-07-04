package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// BillSplit tracks a portion of a POS order assigned to a specific payer.
type BillSplit struct {
	ent.Schema
}

func (BillSplit) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("order_id", uuid.UUID{}),
		field.String("split_label").
			MaxLen(100).
			Comment("e.g. 'Person 1', 'Table A', 'Card ending 4242'"),
		field.Float("amount").
			Comment("Amount assigned to this split"),
		field.JSON("order_line_ids", []string{}).
			Optional().
			Comment("POSOrderLine ids assigned to this split (split-by-item) — drives the per-split receipt."),
		field.String("currency").Default("KES"),
		field.String("status").
			Default("pending").
			Comment("pending | paid | voided"),
		field.String("payment_method").
			Optional().
			Comment("cash | mpesa | card | ..."),
		field.String("external_ref").
			Optional().
			Comment("M-Pesa code or card reference"),
		field.UUID("payment_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("FK to pos_payment when settled"),
	}
}

func (BillSplit) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "order_id"),
		index.Fields("tenant_id", "status"),
	}
}
