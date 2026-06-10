package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// RoomFolioPayment records a payment taken against a room guest's folio at (or before) checkout.
// Multiple payments per folio are supported (partial / split settlement); the folio balance is the
// sum of charges minus the sum of payments. Each payment optionally references the treasury payment
// intent that captured it, for reconciliation.
type RoomFolioPayment struct {
	ent.Schema
}

// Fields of the RoomFolioPayment.
func (RoomFolioPayment) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("room_id", uuid.UUID{}),
		field.UUID("room_guest_id", uuid.UUID{}),
		field.Float("amount"),
		field.String("currency").
			Default("KES"),
		field.String("method").
			Comment("Tender used: cash | card_manual | mpesa | wallet | card | on_account | other"),
		field.String("reference").
			Optional().
			Comment("External reference (M-Pesa code, card approval/PDQ ref, treasury intent id)"),
		field.String("treasury_intent_id").
			Optional().
			Comment("Treasury payment_intent id that captured this payment, when applicable"),
		field.String("status").
			Default("completed").
			Comment("completed | pending | failed"),
		field.UUID("recorded_by", uuid.UUID{}).
			Optional().
			Nillable(),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the RoomFolioPayment.
func (RoomFolioPayment) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "room_guest_id"),
		index.Fields("tenant_id", "room_id"),
	}
}
