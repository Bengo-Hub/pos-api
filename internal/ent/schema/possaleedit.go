package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// SaleEditLineSnapshotJSON is one line's before/after state captured by a sale edit, for the
// audit trail (line-level diff, not just an order-level summary).
type SaleEditLineSnapshotJSON struct {
	LineID     uuid.UUID `json:"line_id,omitempty"`
	SKU        string    `json:"sku"`
	Name       string    `json:"name"`
	Quantity   float64   `json:"quantity"`
	UnitPrice  float64   `json:"unit_price"`
	TotalPrice float64   `json:"total_price"`
}

// POSSaleEdit is the centralized record of one Edit-Sale action on a finalized order — the
// idempotency-bearing reference id the new sale-edit orchestrator mints for every edit
// (treasury GL/AR calls and inventory consumption calls key off this id, NEVER the order id,
// which the original sale's own postings already permanently claim). Mirrors POSReversal's
// shape (real entity + tracked step ledger) for the same reason POSReversal is a real entity
// rather than order metadata: this is the persisted correlation id cross-service calls need.
type POSSaleEdit struct {
	ent.Schema
}

func (POSSaleEdit) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable().
			Comment("THE reference id for GL/AR/inventory idempotency on this edit"),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("order_id", uuid.UUID{}).
			Comment("The finalized order being edited"),
		field.String("order_number").
			Comment("Denormalized human order number"),
		field.Enum("kind").
			Values("reduction", "increase", "mixed", "price_only"),
		field.Bool("fiscalized_at_time").
			Default(false).
			Comment("Policy snapshot: was the order fiscalized when this edit was applied"),
		field.Enum("status").
			Values("pending", "completed", "partial_failure", "failed").
			Default("pending"),
		field.JSON("lines_before", []SaleEditLineSnapshotJSON{}).
			Default([]SaleEditLineSnapshotJSON{}),
		field.JSON("lines_after", []SaleEditLineSnapshotJSON{}).
			Default([]SaleEditLineSnapshotJSON{}),
		field.UUID("linked_reversal_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Set when the non-fiscalized-reduction path ran a reversals.Service partial reversal"),
		field.UUID("linked_return_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Set when the fiscalized-reduction path auto-completed a POSReturn"),
		field.UUID("linked_addendum_order_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Set when the fiscalized-increase path created a linked addendum order"),
		field.JSON("steps", []ReversalStepJSON{}).
			Default([]ReversalStepJSON{}).
			Comment("Reuses POSReversal's step-ledger shape for the in-place-increase sub-steps"),
		field.Float("amount").
			Default(0),
		field.Float("tax_amount").
			Default(0),
		field.Float("cost_amount").
			Default(0),
		field.String("reason").
			NotEmpty(),
		field.UUID("requested_by", uuid.UUID{}),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

func (POSSaleEdit) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "order_id"),
		index.Fields("tenant_id", "status"),
	}
}
