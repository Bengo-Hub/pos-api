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
		field.Float("max_discount_percent").Default(100).Optional().Comment("Max order discount % a cashier may apply without manager approval; above this requires a step-up (100 = no limit)"),
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
		// Cash drawer — the drawer is wired to a receipt printer's RJ11/12 port and opened by an
		// ESC/POS "drawer kick" pulse sent to that printer. The pos-ui sends the kick via the QZ Tray
		// bridge (same channel as printing); these fields configure which printer + when it auto-opens.
		field.Bool("cash_drawer_enabled").Default(false).Optional().Comment("Enable cash-drawer integration (ESC/POS drawer kick via the assigned printer)"),
		field.String("cash_drawer_printer").Optional().Nillable().Comment("OS/QZ-Tray printer name the cash drawer is wired to (RJ11/12). Empty => use the Bill/customer station printer."),
		field.Bool("cash_drawer_auto_open").Default(true).Optional().Comment("Automatically pop the drawer on cash & card settlement"),
		field.String("cash_drawer_kick_code").Default("default").Optional().Comment("ESC/POS drawer-kick pin variant: default (pin2) | pin5 | legacy"),
		// Card terminal / PDQ — manual standalone PDQ is the default (cashier runs the card, enters the
		// approval ref). An integrated mode pushes the amount to a cloud/ECR terminal via a treasury
		// gateway adapter; provider creds live at platform level (treasury GatewayConfig), the
		// terminal identifier (TID/serial) is per-outlet here.
		field.String("card_terminal_mode").Default("manual").Optional().Comment("Card-terminal mode: manual (standalone PDQ + approval ref) | integrated (push amount to terminal)"),
		field.String("card_terminal_provider").Optional().Nillable().Comment("Integrated card-terminal provider key, e.g. pesapal, flutterwave (matches treasury platform gateway)"),
		field.String("card_terminal_tid").Optional().Nillable().Comment("Terminal/serial identifier (TID) of the physical card terminal assigned to this outlet"),
		field.Bool("card_terminal_require_ref").Default(false).Optional().Comment("In manual mode, require the cashier to enter the PDQ approval/reference code before settling"),
		// Payment display fields — shown on receipts when show_payment_info_on_receipt is true
		field.String("mpesa_paybill").Optional().Nillable().Comment("M-PESA Paybill shortcode for customer payments, e.g. 522533"),
		field.String("mpesa_account_reference").Optional().Nillable().Comment("Account reference shown in M-PESA payment prompt, e.g. 79319044"),
		field.String("airtel_money_number").Optional().Nillable().Comment("Airtel Money merchant/paybill number for customer payments"),
		// NOTE FOR INTEGRATOR: run go generate ./ent + atlas migrate diff for mpesa_till/mpesa_pochi
		field.String("mpesa_till").Optional().Nillable().Comment("M-PESA Till (Buy Goods) number"),
		field.String("mpesa_pochi").Optional().Nillable().Comment("M-PESA Pochi la Biashara number"),
		field.String("bank_name").Optional().Nillable().Comment("Bank name for bank transfer payments, e.g. KCB"),
		field.String("bank_account_number").Optional().Nillable().Comment("Bank account number for transfers"),
		field.String("bank_account_name").Optional().Nillable().Comment("Bank account holder name, e.g. THE URBAN LOFT CAFE LIMITED"),
		field.Bool("show_payment_info_on_receipt").Default(false).Optional().Comment("Include payment method section on printed receipts"),
		// Module activation toggles — tenant admin controls which modules are active per outlet
		field.Bool("hotel_module_enabled").Default(false).Optional().Comment("Hotel/room management module (hospitality use case)"),
		field.Bool("layaway_enabled").Default(false).Optional().Comment("Layaway plan / instalment payment module"),
		field.Bool("shift_reports_enabled").Default(false).Optional().Comment("Shift reports & daily closing module"),
		// Shift duration enforcement
		field.Bool("shift_auto_end_enabled").Default(false).Optional().Comment("Automatically end shift after shift_max_hours to prevent forgotten open sessions"),
		field.Int("shift_max_hours").Default(12).Optional().Comment("Maximum shift length in hours before auto-end (1–24, default 12)"),
		field.Int("table_max_occupation_minutes").
			Default(240).
			Optional().
			Comment("Minutes before an occupied table is flagged for aging. 0 = disabled. Default 4 hours."),
		field.UUID("default_warehouse_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Inventory warehouse ID used for stock deduction on pos.sale.finalized events"),
		field.Int("return_window_days").
			Default(30).
			Optional().
			Comment("Max days after purchase to allow returns. 0 = no limit."),
		// Hybrid catalog support — a single outlet often sells across more than one business
		// vertical (e.g. a hospitality cafe that also sells co-working/conference SERVICE
		// packages). The outlet's primary `use_case` still drives the default catalog
		// type/category allow-list (assembleMenuItems); this field ADDITIONALLY unions in the
		// item types + categories allowed for each listed use_case, so hybrid items reach the
		// same terminal without reclassifying the outlet. Empty = unchanged legacy behavior
		// (primary use_case only).
		field.JSON("catalog_use_cases", []string{}).
			Default([]string{}).
			Optional().
			Comment("Extra use_cases (beyond the outlet's primary use_case) whose item types/categories are also allowed on this outlet's POS catalog — enables hybrid selling."),
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
