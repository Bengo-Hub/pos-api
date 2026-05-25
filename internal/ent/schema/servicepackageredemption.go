package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ServicePackageRedemption records each session redeemed from a purchased package.
type ServicePackageRedemption struct {
	ent.Schema
}

func (ServicePackageRedemption) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("purchase_id", uuid.UUID{}).Comment("FK to ServicePackagePurchase"),
		field.UUID("pos_order_id", uuid.UUID{}).Optional().Nillable().Comment("POS order for this redemption visit"),
		field.UUID("redeemed_by", uuid.UUID{}).Comment("Staff member who performed the service"),
		field.UUID("service_catalog_item_id", uuid.UUID{}).Optional().Nillable().Comment("Service item redeemed"),
		field.Time("redeemed_at").Default(time.Now).Immutable(),
	}
}

func (ServicePackageRedemption) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "purchase_id"),
		index.Fields("tenant_id", "redeemed_at"),
	}
}
