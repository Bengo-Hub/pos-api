package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ReversalLineJSON is one sale line (or part of one) selected for reversal.
type ReversalLineJSON struct {
	LineID uuid.UUID `json:"line_id"`
	SKU    string    `json:"sku"`
	Name   string    `json:"name"`
	// Quantity being reversed; OfQuantity is the line's original quantity so downstream
	// services can prorate (inventory consumption ratio, tax proration).
	Quantity   float64 `json:"quantity"`
	OfQuantity float64 `json:"of_quantity"`
	Amount     float64 `json:"amount"`
	TaxAmount  float64 `json:"tax_amount,omitempty"`
}

// ReversalStepJSON tracks one cross-service step of a reversal so the sync-monitor
// "Txn Reversals" tab can show exactly what happened (and retry what failed).
type ReversalStepJSON struct {
	Step    string `json:"step"`    // pos_totals | inventory_consumption | treasury_gl | etims_credit_note
	Service string `json:"service"` // pos | inventory | treasury
	Status  string `json:"status"`  // pending | completed | failed | skipped
	Detail  string `json:"detail,omitempty"`
	Ref     string `json:"ref,omitempty"` // cross-service reference (refund id, reversal consumption id, credit-note no.)
	At      string `json:"at,omitempty"`  // RFC3339 completion time
}

// POSReversal is a platform-owner data-repair transaction: it reverses a FINALIZED sale
// (whole order or individual items) across every integrated service — POS totals/void,
// inventory BOM consumption, treasury GL, and the KRA eTIMS credit note when the sale was
// fiscalised. Distinct from POSReturn (the customer-facing three-stage returns lifecycle):
// a reversal corrects the original order record itself and is initiated by the platform
// owner on tenant request.
type POSReversal struct {
	ent.Schema
}

func (POSReversal) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("order_id", uuid.UUID{}).
			Comment("The finalized order being reversed"),
		field.String("order_number").
			Comment("Denormalized human order number for the reversals list"),
		field.String("reversal_number").
			NotEmpty().
			Comment("Human reference (REV-...) shown on treasury documents"),
		field.Enum("scope").
			Values("full", "partial"),
		field.Enum("status").
			Values("pending", "completed", "partial_failure", "failed").
			Default("pending"),
		field.String("reason").
			NotEmpty(),
		field.String("refund_channel").
			Default("cash").
			Comment("Treasury settlement channel: cash|mpesa|bank|cheque|store_credit|offset_invoice"),
		field.JSON("lines", []ReversalLineJSON{}).
			Default([]ReversalLineJSON{}).
			Comment("Sale lines reversed; empty only while scope=full is being resolved"),
		field.Float("amount").
			Default(0).
			Comment("Gross sale value reversed (incl. tax)"),
		field.Float("tax_amount").
			Default(0),
		field.Float("cost_amount").
			Default(0).
			Comment("COGS passed to treasury for reversal (catalog cost source, symmetric with sale-time posting)"),
		field.JSON("steps", []ReversalStepJSON{}).
			Default([]ReversalStepJSON{}),
		field.String("idempotency_key").
			Optional().
			Nillable(),
		field.UUID("requested_by", uuid.UUID{}).
			Comment("Platform-owner principal who ran the reversal"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

func (POSReversal) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "reversal_number").Unique(),
		index.Fields("tenant_id", "order_id"),
		index.Fields("tenant_id", "status"),
		index.Fields("idempotency_key").Unique(),
	}
}
