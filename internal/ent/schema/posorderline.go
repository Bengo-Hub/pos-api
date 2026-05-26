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
		field.UUID("catalog_item_id", uuid.UUID{}),
		field.String("sku").
			NotEmpty(),
		field.String("name").
			NotEmpty(),
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
