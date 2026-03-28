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
		field.String("display_mode").Default("card").Optional().Comment("list=supermarket/hardware, card=restaurant, image_grid=bar/lounge"),
		field.Bool("show_images").Default(true).Optional().Comment("Show item images in catalog view"),
		field.Bool("show_barcode_scanner").Default(false).Optional().Comment("Show barcode scanner input for retail"),
		field.String("default_view").Default("catalog").Optional().Comment("catalog, quick_sale, tables, appointments"),
		field.Bool("enable_kds").Default(false).Optional().Comment("Kitchen Display System for hospitality"),
		field.Bool("enable_appointments").Default(false).Optional().Comment("Appointment booking for salons/services"),
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
