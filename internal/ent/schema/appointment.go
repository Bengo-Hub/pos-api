package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Appointment holds the schema definition for the Appointment entity.
type Appointment struct {
	ent.Schema
}

// Fields of the Appointment.
func (Appointment) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("customer_id", uuid.UUID{}).Optional().Nillable().Comment("Customer user ID from auth-service"),
		field.String("customer_name").Optional(),
		field.String("customer_phone").Optional(),
		field.UUID("staff_member_id", uuid.UUID{}).Optional().Nillable().Comment("Assigned staff"),
		field.UUID("service_item_id", uuid.UUID{}).Comment("Inventory item of type SERVICE"),
		field.String("service_sku").NotEmpty(),
		field.Time("start_time").Comment("Appointment start"),
		field.Time("end_time").Comment("Appointment end = start + service duration"),
		field.Enum("status").Values("scheduled", "confirmed", "in_progress", "completed", "cancelled", "no_show").Default("scheduled"),
		field.Text("notes").Optional(),
		field.UUID("pos_order_id", uuid.UUID{}).Optional().Nillable().Comment("Linked POS order for payment"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

// Indexes of the Appointment.
func (Appointment) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id", "start_time"),
		index.Fields("tenant_id", "staff_member_id", "start_time"),
		index.Fields("status"),
	}
}
