package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// LostFoundItem tracks a guest item found on the property — from the moment housekeeping/front
// desk logs it, through storage, to being claimed by its owner or disposed of. Not tied to an
// active RoomGuest stay (items are commonly found after checkout, or in common areas with no
// room at all), unlike RoomFolioItem/RoomDamageReport which always need one.
type LostFoundItem struct {
	ent.Schema
}

func (LostFoundItem) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("room_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Where it was found, if in a guest room; nil for common areas (lobby, pool, restaurant, etc.)"),
		field.UUID("room_guest_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("The stay it was found during/just after, if known — used to prefill guest contact details"),
		field.String("description").
			NotEmpty(),
		field.Enum("category").
			Values("electronics", "clothing", "jewelry", "documents", "toiletries", "luggage", "other").
			Default("other"),
		field.String("location_found").
			Optional().
			Comment("Free text: e.g. 'under the bed', 'poolside sun lounger', 'lobby restroom'"),
		field.String("storage_location").
			Optional().
			Comment("Where it's being kept: e.g. 'Front desk safe', 'Lost & Found box 3'"),
		field.JSON("photo_urls", []string{}).
			Optional().
			Comment("Object-storage KEYS for photos (same convention as RoomDamageReport.evidence_urls) — never store blobs inline"),
		field.Enum("status").
			Values("stored", "claimed", "disposed", "donated").
			Default("stored"),
		field.UUID("found_by", uuid.UUID{}).
			Comment("user_id ref from auth-service"),
		field.Time("found_at").
			Default(time.Now),
		// Guest contact details — captured directly (not just via room_guest_id) so an item
		// found in a common area with no room/guest link can still be followed up on, and so a
		// staff-typed correction doesn't require editing RoomGuest itself.
		field.String("guest_name").
			Optional(),
		field.String("guest_phone").
			Optional(),
		field.String("guest_email").
			Optional(),
		field.String("claimed_by_name").
			Optional().
			Comment("Who actually collected it — may differ from guest_name (family member, agent)"),
		field.String("claimed_notes").
			Optional(),
		field.Time("claimed_at").
			Optional().
			Nillable(),
		field.String("disposal_reason").
			Optional(),
		field.Time("disposed_at").
			Optional().
			Nillable(),
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

func (LostFoundItem) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id"),
		index.Fields("tenant_id", "status"),
		index.Fields("tenant_id", "room_id"),
	}
}
