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
		field.UUID("inventory_item_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Stable inventory-api Item UUID (preferred over sku string for joins; survives SKU renames)"),
		field.String("item_use_case").
			Optional().
			Comment("Synced from inventory item: RETAIL/PHARMACY/FOOD_BEVERAGE/HOSPITALITY_ROOM/HOSPITALITY_FACILITY/CONFERENCE/SALON_SERVICE/AMENITY"),
		field.Bool("is_bundle").
			Default(false).
			Comment("True when this catalog entry is an inventory Bundle (package), e.g. conference DDR/RDR"),

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

		// Tax config sourced from inventory item and treasury TaxCode.
		// Synced when catalog updates arrive via NATS inventory.catalog.updated.
		field.String("tax_code_id").
			Optional().
			Comment("Treasury TaxCode.code (e.g. VAT-16) — used for computing tax at sale time"),
		field.Bool("price_includes_tax").
			Default(false).
			Comment("True when selling_price already includes the tax amount (VAT-inclusive pricing)"),

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

		// KDS routing — managers assign which preparation station handles this item.
		// Overrides category_filter matching. nil = use category_filter on KDSStation.
		field.UUID("kds_station_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Explicit KDS station for this item; overrides category_filter matching"),

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
		index.Fields("tenant_id", "inventory_item_id"),
	}
}
