package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// POSSaleShred is the permanent audit record of a hard-deleted (non-fiscalised) sale — the
// "shred receipt" the tenant-admin Delete-Sale tool leaves behind once it has physically removed
// a POSOrder and its lines/payments. Deliberately carries NO edge/FK to POSOrder: the whole point
// is that it must keep resolving after the order row it describes no longer exists. See
// internal/modules/saledelete (built on top of internal/modules/reversals, the same engine the
// platform-owner Txn Reversal tool uses for the fiscalised branch, which never hard-deletes).
type POSSaleShred struct {
	ent.Schema
}

func (POSSaleShred) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("order_id", uuid.UUID{}).
			Comment("The now-deleted order's original id — NOT an edge, the row it names is gone"),
		field.String("order_number").
			Comment("Denormalized human order number, survives the order's deletion"),
		field.String("reason").
			NotEmpty(),
		field.Enum("status").
			Values("pending", "completed", "partial_failure", "failed").
			Default("pending"),
		// Full pre-deletion snapshot (order header + lines + payments + the treasury ledger rows
		// and inventory consumption rows the shred touched) — the durable "audit trail kept"
		// artifact even though the live order/ledger rows themselves are physically removed.
		field.JSON("snapshot", map[string]any{}).
			Default(map[string]any{}),
		// Reuses the same step-ledger shape the reversal engine already defines (step/service/
		// status/detail/ref/at) — one consistent tracked-step model across both tools.
		field.JSON("steps", []ReversalStepJSON{}).
			Default([]ReversalStepJSON{}),
		field.String("idempotency_key").
			Optional().
			Nillable(),
		field.UUID("requested_by", uuid.UUID{}).
			Comment("Tenant admin (or platform owner) who ran the shred"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

func (POSSaleShred) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "order_number"),
		index.Fields("tenant_id", "order_id"),
		index.Fields("idempotency_key").Unique(),
	}
}
