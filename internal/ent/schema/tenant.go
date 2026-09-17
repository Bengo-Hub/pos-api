package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Tenant holds the schema definition for the Tenant entity.
//
// Auth-api is the single source of truth for tenant identity, branding, and subscription data.
// Downstream services store only the minimal reference needed for FK relationships and routing.
type Tenant struct {
	ent.Schema
}

// Fields of the Tenant.
// Auth-api is the single source of truth for tenant identity, branding, and subscription data.
// Downstream services store only the minimal reference needed for FK relationships and routing.
// All other tenant data (branding, contact info, subscription) is fetched from auth-api on demand
// or read from JWT claims (subscription plan/status/limits).
func (Tenant) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("name").
			NotEmpty().
			Comment("Display name — synced from auth-api"),
		field.String("slug").
			NotEmpty().
			Unique().
			Comment("URL-safe identifier — synced from auth-api"),
		field.String("status").
			Default("active").
			Comment("Tenant status: active | inactive | suspended"),
		field.String("use_case").
			Optional().
			Nillable().
			Comment("Primary business use case — synced from auth-api"),
		field.String("timezone").
			Default("Africa/Nairobi").
			Comment("IANA timezone (day/shift/report boundaries) — synced from auth-api"),
		field.String("sync_status").
			Default("synced").
			Comment("Sync status from auth-api: synced | pending | failed"),
		field.Time("last_sync_at").
			Optional().
			Nillable().
			Comment("Last successful sync from auth-api"),
		field.Time("maintenance_starts_at").
			Optional().
			Nillable().
			Comment("Scheduled start of a maintenance window (product name: Repair Mode). While now is within [maintenance_starts_at, maintenance_ends_at], every non-platform-owner request to this tenant's POS is blocked with an under-maintenance response. Owned locally by pos-api, not synced from auth-api. Null means no window scheduled."),
		field.Time("maintenance_ends_at").
			Optional().
			Nillable().
			Comment("Scheduled end of the maintenance window. The lockout auto-resumes normal access once now passes this timestamp, with no manual step to turn it back off."),
		field.String("maintenance_reason").
			Optional().
			Nillable().
			Comment("Free-text reason shown on the under-maintenance banner for the current or most recent window"),
		field.String("maintenance_activated_by").
			Optional().
			Nillable().
			Comment("Platform owner email/identifier who scheduled the current or most recent maintenance window"),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the Tenant.
func (Tenant) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("users", User.Type),
		edge.To("outlets", Outlet.Type),
	}
}

// Indexes of the Tenant.
func (Tenant) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("slug").Unique(),
		index.Fields("status"),
	}
}
