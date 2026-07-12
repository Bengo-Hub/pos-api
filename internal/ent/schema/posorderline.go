package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// POSOrderLine holds the schema definition for the POSOrderLine entity.
type POSOrderLine struct {
	ent.Schema
}

// Fields of the POSOrderLine.
func (POSOrderLine) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("order_id", uuid.UUID{}),
		// When this specific line was added to the bill. Happy-hour eligibility is decided
		// per-line by THIS timestamp (not the order's create time or the payment time), so a
		// drink rung up during the window earns the deal even on a tab opened earlier — and one
		// added before the window does not. Optional/Nillable so existing rows (backfilled to the
		// order's created_at) and any client that omits it don't break; set to now on every create.
		field.Time("created_at").
			Optional().
			Nillable().
			Immutable().
			Comment("Timestamp this line was added to the order; drives per-line happy-hour window eligibility"),
		field.UUID("catalog_item_id", uuid.UUID{}),
		field.String("sku").
			NotEmpty(),
		field.String("name").
			NotEmpty(),
		field.String("category").
			Optional().
			Comment("Item category name at sale time; used for KDS station routing (kitchen vs bar)"),
		field.Float("quantity"),
		field.Float("unit_price"),
		field.Float("total_price"),
		field.Int("weight_grams").
			Optional().
			Nillable().
			Comment("Weight at sale time for weighed items"),
		field.String("lot_number").
			Optional().
			Comment("Lot/batch number if item tracks lots"),
		field.Time("expiry_date").
			Optional().
			Nillable().
			Comment("Expiry date from lot if applicable"),
		field.String("serial_number").
			Optional().
			Nillable().
			Comment("Serial number captured at point of sale for tracked items"),
		field.Float("partial_units").
			Optional().
			Nillable().
			Comment("Partial pack decimal quantity (e.g. 10 of 30 tablets dispensed)"),
		// Tax fields — computed once at order line creation from catalog tax config + treasury rate.
		// If price_includes_tax=true: tax_amount is back-calculated from unit_price.
		// If price_includes_tax=false: tax_amount is additive on top of unit_price.
		field.String("tax_code_id").
			Optional().
			Comment("Treasury TaxCode.code applied to this line (e.g. VAT-16, EXM)"),
		field.String("tax_kra_code").
			Optional().
			Comment("KRA eTIMS TaxTyCd (A=16%VAT, B=8%VAT, C=excise, D=exempt, E=zero)"),
		field.Float("tax_rate").
			Optional().
			Nillable().
			Comment("Tax rate percentage applied (e.g. 16.0)"),
		field.Float("tax_amount").
			Optional().
			Nillable().
			Comment("Computed tax amount for the total line (quantity × unit tax)"),
		field.Bool("price_includes_tax").
			Default(false).
			Comment("True when unit_price is VAT-inclusive; tax_amount is back-calculated"),
		field.Int("course_number").
			Default(0).
			Comment("Course firing order: 0=immediate, 1=starter, 2=main, 3=dessert. KDS hides items with course_number > order.fired_courses."),
		// KDS routing — resolved at order creation from POSCatalogOverride.kds_station_id.
		// nil = unrouted (goes to expo/all stations or default station).
		field.UUID("kds_station_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("KDS station this line is routed to; copied from POSCatalogOverride at order creation"),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
		// Void fields — set when a line is removed from a sent/persisted order
		// (anti-sweethearting). Pre-send cart edits are client-only; once a line
		// is server-persisted, removing it soft-voids the line (kept for audit)
		// rather than hard-deleting it.
		field.Float("voided_qty").
			Optional().
			Nillable().
			Comment("Quantity voided from this line (full or partial)"),
		field.String("voided_reason").
			Optional().
			Nillable(),
		field.UUID("voided_by", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Time("voided_at").
			Optional().
			Nillable(),
	}
}

// Edges of the POSOrderLine.
func (POSOrderLine) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("order", POSOrder.Type).
			Ref("lines").
			Field("order_id").
			Unique().
			Required(),
		edge.To("modifiers", POSLineModifier.Type),
	}
}
