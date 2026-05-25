package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ServicePackagePurchase tracks a client's purchase of a ServicePackage.
type ServicePackagePurchase struct {
	ent.Schema
}

func (ServicePackagePurchase) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("package_id", uuid.UUID{}).Comment("FK to ServicePackage"),
		field.String("client_name").NotEmpty(),
		field.String("client_phone").NotEmpty(),
		field.UUID("pos_order_id", uuid.UUID{}).Optional().Nillable().Comment("Initial purchase POS order"),
		field.Int("sessions_used").Default(0),
		field.Int("sessions_remaining"),
		field.Time("expires_at"),
		field.String("status").Default("active").Comment("active | exhausted | expired | cancelled"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (ServicePackagePurchase) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "client_phone"),
		index.Fields("tenant_id", "package_id"),
		index.Fields("tenant_id", "status"),
	}
}
