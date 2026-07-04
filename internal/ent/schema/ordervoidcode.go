package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// OrderVoidCode is a one-time, order-scoped authorization code a manager generates (from their own
// login, possibly remotely) so a waiter/cashier can void a specific bill when the manager is not
// physically present to scan a card or enter a PIN. The plaintext code is shown once to the manager
// and shared with the cashier; only its hash is stored. Single-use and time-limited.
type OrderVoidCode struct {
	ent.Schema
}

// Fields of the OrderVoidCode.
func (OrderVoidCode) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("order_id", uuid.UUID{}).
			Comment("The specific POS order this code authorizes voiding."),
		field.String("action").
			Default("order.void").
			Comment("Sensitive action this code authorizes (order.void)."),
		field.String("code_hash").
			Sensitive().
			Comment("bcrypt hash of the one-time code (plaintext is never stored)."),
		field.UUID("approver_user_id", uuid.UUID{}).
			Comment("Manager (override role) who generated the code."),
		field.String("approver_name").
			Optional(),
		field.String("reason").
			Optional().
			Comment("Optional note from the manager on why the void is authorized."),
		field.Time("expires_at"),
		field.Time("used_at").
			Optional().
			Nillable().
			Comment("Set when the code is redeemed — enforces single use."),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the OrderVoidCode.
func (OrderVoidCode) Indexes() []ent.Index {
	return []ent.Index{
		// Fast lookup of active codes for an order when redeeming.
		index.Fields("tenant_id", "order_id", "action"),
	}
}
