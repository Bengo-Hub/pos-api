package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// StaffMember holds the schema definition for the StaffMember entity.
type StaffMember struct {
	ent.Schema
}

// Fields of the StaffMember.
func (StaffMember) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("user_id", uuid.UUID{}).Comment("Auth-service user ID"),
		field.String("name").NotEmpty(),
		field.JSON("service_skus", []string{}).Optional().Comment("Service SKUs this staff can perform"),
		field.JSON("working_hours", map[string]any{}).Optional().Comment("Weekly schedule"),
		field.Float("commission_rate").Optional().Nillable().Comment("Commission percentage on services"),
		field.Bool("is_active").Default(true),
		field.String("role").Default("cashier").
			Comment("POS role: admin (unrestricted)|manager (RBAC scoped)|cashier|waiter|kitchen|bar|receptionist|pharmacist|stylist|therapist"),
		// Employment & compensation
		field.Enum("employment_type").
			Values("full_time", "part_time", "casual", "contractor").
			Default("full_time").Optional(),
		field.Float("hourly_rate").Optional().Nillable().
			Comment("Compensation per hour (for hourly/casual staff)"),
		field.Float("daily_rate").Optional().Nillable().
			Comment("Compensation per day (for day-rate staff)"),
		field.Float("monthly_salary").Optional().Nillable().
			Comment("Fixed monthly gross salary"),
		field.String("mpesa_phone").Optional().Nillable().
			Comment("M-Pesa phone (254...) for salary disbursement"),
		field.String("bank_account_number").Optional().Nillable(),
		field.String("bank_name").Optional().Nillable(),
		// Terminal PIN login — bcrypt hash of the 4-6 digit PIN set by a manager.
		// Null means PIN not configured; staff must use SSO until a PIN is set.
		field.String("pin_hash").Optional().Nillable().Sensitive(),
		// hex(SHA256(tenantID+":"+outletID+":"+pin)) — indexed for O(1) PIN-first lookup.
		// Scoped to tenant+outlet so the same PIN digits can belong to different staff at different outlets.
		field.String("pin_fast_hash").Optional().Nillable().Sensitive(),
		// Brute-force protection: count of consecutive wrong PINs since last success.
		field.Int("pin_failed_attempts").Default(0),
		// When non-nil, PIN login is locked until this time (set after 5 failed attempts).
		field.Time("pin_locked_until").Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

// Indexes of the StaffMember.
func (StaffMember) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id"),
		index.Fields("tenant_id", "user_id").Unique(),
		index.Fields("tenant_id", "outlet_id", "pin_fast_hash").Unique(),
	}
}
