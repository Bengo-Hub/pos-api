package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// RoomDamageReport tracks a reported guest-caused damage/fine against a room, from the
// moment it is logged (with optional photo evidence) through manager approval or rejection.
// Approval posts the charge to the guest's folio as a RoomFolioItem (charge_type=damage) via
// the same PostFolioCharge path used for any other charge — this entity is the workflow state
// BEFORE that charge exists, not a replacement for RoomFolioItem's append-only ledger.
type RoomDamageReport struct {
	ent.Schema
}

func (RoomDamageReport) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("room_id", uuid.UUID{}),
		field.UUID("room_guest_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("The stay this damage was reported against; nil when reported after the guest already checked out"),
		field.String("description").
			NotEmpty(),
		field.Float("amount").
			Min(0).
			Comment("Estimated/assessed cost — the amount posted to the folio on approval"),
		field.String("currency").
			Default("KES"),
		field.JSON("evidence_urls", []string{}).
			Optional().
			Comment("Object-storage KEYS for photo evidence (uploaded via the platform media service, same pattern as RoomGuest.id_document_url) — never store blobs inline"),
		field.Enum("status").
			Values("pending", "approved", "rejected").
			Default("pending"),
		field.UUID("reported_by", uuid.UUID{}).
			Comment("user_id ref from auth-service"),
		field.UUID("reviewed_by", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("user_id ref from auth-service — the manager who approved/rejected"),
		field.Time("reviewed_at").
			Optional().
			Nillable(),
		field.String("review_notes").
			Optional(),
		field.UUID("folio_item_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Set once approved — the RoomFolioItem the charge was posted as"),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

func (RoomDamageReport) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "room_id"),
		index.Fields("tenant_id", "status"),
		index.Fields("tenant_id", "room_guest_id"),
	}
}
