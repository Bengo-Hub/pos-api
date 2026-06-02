package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// RoomFolioItem represents a charge posted to a room's folio during a guest stay.
type RoomFolioItem struct {
	ent.Schema
}

// Fields of the RoomFolioItem.
func (RoomFolioItem) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("room_id", uuid.UUID{}),
		field.UUID("room_guest_id", uuid.UUID{}),
		field.String("description").
			NotEmpty(),
		field.Float("amount").
			Min(0),
		field.String("currency").
			Default("KES"),
		field.Enum("charge_type").
			Values("room_charge", "food", "laundry", "minibar", "room_service",
				"amenity", "facility", "late_checkout", "damage",
				"package", "conference", "meal_voucher", "other").
			Default("other"),
		field.String("inventory_sku").
			Optional().
			Comment("inventory-api SKU when this charge corresponds to a catalog item — drives stock backflush"),
		field.UUID("inventory_bundle_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("inventory-api Bundle id when this charge is a package (conference/room rate plan)"),
		field.UUID("pos_order_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Linked POS order if charge originated from an order"),
		field.UUID("created_by", uuid.UUID{}).
			Comment("user_id ref from auth-service"),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Edges of the RoomFolioItem.
func (RoomFolioItem) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("room", Room.Type).Ref("folio_items").Field("room_id").Unique().Required(),
		edge.From("guest", RoomGuest.Type).Ref("folio_items").Field("room_guest_id").Unique().Required(),
	}
}

// Indexes of the RoomFolioItem.
func (RoomFolioItem) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "room_id"),
		index.Fields("tenant_id", "room_guest_id"),
	}
}
