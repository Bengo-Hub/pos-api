package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// POSPermission holds the schema definition for POS service permissions.
type POSPermission struct {
	ent.Schema
}

// Fields of the POSPermission.
func (POSPermission) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("permission_code").
			NotEmpty().
			Unique().
			Comment("Permission code: pos.orders.add, pos.payments.view, etc."),
		field.String("name").
			NotEmpty().
			Comment("Display name"),
		field.String("module").
			NotEmpty().
			Comment("Module: orders, payments, catalog, outlets, devices, sessions, cash_drawers, tables, gift_cards, price_books, modifiers, channels, config, users"),
		field.String("action").
			NotEmpty().
			Comment("Action: add, view, view_own, change, change_own, delete, delete_own, manage, manage_own"),
		field.String("resource").
			Optional().
			Comment("Resource: orders, payments, etc."),
		field.Text("description").
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Edges of the POSPermission.
func (POSPermission) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("roles", POSRoleV2.Type).Ref("permissions").Through("role_permissions", POSRolePermission.Type),
	}
}

// Indexes of the POSPermission.
func (POSPermission) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("permission_code").Unique(),
		index.Fields("module"),
		index.Fields("action"),
		index.Fields("module", "action"),
	}
}
