package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ClientRecord stores POS-specific client data. Contact master data (name, email, dob, gender)
// is owned exclusively by MarketFlow CRM — only crm_contact_id is stored here as a FK.
type ClientRecord struct {
	ent.Schema
}

func (ClientRecord) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("crm_contact_id", uuid.UUID{}).Optional().Nillable().Comment("FK to MarketFlow CRM contact — nullable until linked"),
		field.String("phone").NotEmpty().Comment("Primary lookup key per tenant; synced from CRM on link"),
		field.String("notes").Optional().Comment("POS-specific stylist notes, service history notes"),
		field.JSON("preferences", map[string]any{}).Optional().Comment("POS-specific: preferred stylist, allergens, product preferences"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (ClientRecord) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "phone").Unique(),
		index.Fields("tenant_id", "crm_contact_id"),
		index.Fields("tenant_id", "outlet_id"),
	}
}
