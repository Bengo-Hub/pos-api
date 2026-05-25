package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ServicePackage is a pre-purchasable session bundle (e.g. 10 massages).
type ServicePackage struct {
	ent.Schema
}

func (ServicePackage) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}).Optional().Nillable(),
		field.String("name").NotEmpty(),
		field.String("description").Optional(),
		field.Float("price"),
		field.String("currency").Default("KES"),
		field.Int("sessions_total").Comment("Total sessions included in package"),
		field.Int("validity_days").Default(365).Comment("Days from purchase before package expires"),
		field.JSON("applicable_services", []string{}).Optional().Comment("Catalog item IDs this package applies to"),
		field.Bool("is_active").Default(true),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (ServicePackage) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "is_active"),
		index.Fields("tenant_id", "outlet_id"),
	}
}
