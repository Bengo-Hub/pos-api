package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AuditLog is a centralized, append-only trail of sensitive / fraud-relevant
// POS actions (voids, line removals, discount/price overrides, refunds, cash
// drawer no-sale / pay-in / pay-out / cash-drop, role changes). Immutable.
type AuditLog struct {
	ent.Schema
}

// Fields of the AuditLog.
func (AuditLog) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}).Optional().Nillable().
			Comment("Outlet scope of the action, when applicable"),
		field.UUID("actor_user_id", uuid.UUID{}).
			Comment("User who performed the action"),
		field.UUID("actor_staff_id", uuid.UUID{}).Optional().Nillable().
			Comment("Staff member (PIN identity) who performed the action, when applicable"),
		field.UUID("approver_user_id", uuid.UUID{}).Optional().Nillable().
			Comment("Manager who approved via PIN step-up, when required"),
		field.String("action").NotEmpty().
			Comment("Dotted action code, e.g. order.void, drawer.pay_out"),
		field.String("entity_type").Optional(),
		field.String("entity_id").Optional(),
		field.Text("reason").Optional(),
		field.JSON("before_json", map[string]any{}).Optional(),
		field.JSON("after_json", map[string]any{}).Optional(),
		field.Float("amount").Optional().Nillable().
			Comment("Monetary magnitude of the action, when relevant"),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

// Indexes of the AuditLog.
func (AuditLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "created_at"),
		index.Fields("tenant_id", "outlet_id", "action"),
		index.Fields("tenant_id", "actor_user_id"),
	}
}
