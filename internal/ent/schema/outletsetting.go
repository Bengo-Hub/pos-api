package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// OutletSetting holds the schema definition for the OutletSetting entity.
type OutletSetting struct {
	ent.Schema
}

// Fields of the OutletSetting.
func (OutletSetting) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("outlet_id", uuid.UUID{}),
		field.JSON("receipts_json", map[string]any{}).
			Optional(),
		field.JSON("tax_config_json", map[string]any{}).
			Optional(),
		field.JSON("service_charge_json", map[string]any{}).
			Optional(),
		field.JSON("opening_hours_json", map[string]any{}).
			Optional(),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
		field.String("pin_login_message").Optional().Nillable().Comment("Admin-set message shown on PIN login screen, e.g. 'Shift starts 8AM'"),
		field.String("screensaver_url").Optional().Nillable().Comment("Custom screensaver URL for idle terminal"),
		field.String("display_mode").Default("card").Optional().Comment("list=supermarket/hardware, card=restaurant, image_grid=bar/lounge"),
		field.Bool("show_images").Default(true).Optional().Comment("Show item images in catalog view"),
		field.Bool("show_barcode_scanner").Default(false).Optional().Comment("Show barcode scanner input for retail"),
		field.String("default_view").Default("catalog").Optional().Comment("catalog, quick_sale, tables, appointments"),
		field.Bool("enable_kds").Default(false).Optional().Comment("Kitchen Display System for hospitality"),
		field.Bool("enable_appointments").Default(false).Optional().Comment("Appointment booking for salons/services"),
		// Receipt & printing settings
		field.String("receipt_header").Optional().Nillable().Comment("Custom header text printed on receipts"),
		field.String("receipt_footer").Optional().Nillable().Comment("Custom footer text (e.g. return policy) printed on receipts"),
		field.String("currency").Default("KES").Optional().Comment("ISO 4217 currency code for this outlet"),
		field.Bool("vat_enabled").Default(true).Optional().Comment("Whether to apply VAT on orders"),
		field.Float("vat_rate").Default(16.0).Optional().Comment("VAT percentage rate, e.g. 16.0 for 16%"),
		field.String("printer_type").Default("thermal").Optional().Comment("thermal | network | bluetooth | none"),
		field.String("printer_ip").Optional().Nillable().Comment("Network printer IP address (only for printer_type=network)"),
		field.String("paper_width").Default("80mm").Optional().Comment("Receipt paper width: 58mm | 80mm"),
		field.Bool("auto_print_order").Default(false).Optional().Comment("Automatically print receipt when order is completed"),
		field.Bool("auto_print_kitchen").Default(false).Optional().Comment("Automatically print kitchen ticket on order creation"),
		field.JSON("printer_profiles", []map[string]any{}).
			Default([]map[string]any{}).
			Optional().
			Comment("Array of printer profile objects: [{id, label, printer_type, printer_ip, paper_width, auto_print, categories}]"),
		// Module activation toggles — tenant admin controls which modules are active per outlet
		field.Bool("hotel_module_enabled").Default(false).Optional().Comment("Hotel/room management module (hospitality use case)"),
		field.Bool("layaway_enabled").Default(false).Optional().Comment("Layaway plan / instalment payment module"),
		field.Bool("shift_reports_enabled").Default(false).Optional().Comment("Shift reports & daily closing module"),
		// Shift duration enforcement
		field.Bool("shift_auto_end_enabled").Default(false).Optional().Comment("Automatically end shift after shift_max_hours to prevent forgotten open sessions"),
		field.Int("shift_max_hours").Default(12).Optional().Comment("Maximum shift length in hours before auto-end (1–24, default 12)"),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the OutletSetting.
func (OutletSetting) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("outlet", Outlet.Type).
			Ref("settings").
			Field("outlet_id").
			Unique().
			Required(),
	}
}
