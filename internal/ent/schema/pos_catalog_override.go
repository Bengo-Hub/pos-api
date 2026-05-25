package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// POSCatalogOverride holds POS-specific overrides for inventory items.
// Inventory-api is the source of truth for item data (name, description, image, category).
// This table only stores POS-specific pricing/availability/compliance overrides per outlet.
type POSCatalogOverride struct {
	ent.Schema
}

// Fields of the POSCatalogOverride.
func (POSCatalogOverride) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("null = applies to all outlets for this tenant"),
		field.String("inventory_sku").
			NotEmpty().
			Comment("SKU from inventory-api — join key"),

		// POS pricing override — takes precedence over inventory-api pricing tiers
		field.Float("selling_price").
			Optional().
			Nillable().
			Comment("POS retail price override; nil = use inventory-api tier price"),
		field.String("currency").
			Default("KES"),
		field.String("tax_status").
			Default("taxable").
			Comment("taxable, tax_exempt, zero_rated"),

		// Availability
		field.Bool("is_available").
			Default(true),
		field.Bool("is_featured").
			Default(false),
		field.Int("display_order").
			Default(0),

		// Compliance flags (sourced from inventory; POS can override per-outlet)
		field.Bool("requires_prescription").
			Default(false),
		field.Bool("is_returnable").
			Default(true),
		field.Bool("requires_age_verification").
			Default(false),
		field.Bool("is_controlled_substance").
			Default(false),
		field.Int("minimum_age").
			Optional().
			Nillable(),

		// Appointment/service fields
		field.Int("duration_minutes").
			Optional().
			Nillable().
			Comment("Service duration for salon/appointment flow"),

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

// Indexes of the POSCatalogOverride.
func (POSCatalogOverride) Indexes() []ent.Index {
	return []ent.Index{
		// Unique override per tenant+outlet+sku (outlet nil = tenant-wide)
		index.Fields("tenant_id", "inventory_sku", "outlet_id"),
		index.Fields("tenant_id", "outlet_id"),
	}
}
