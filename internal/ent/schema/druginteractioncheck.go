package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// DrugInteractionCheck holds the schema definition for the DrugInteractionCheck entity.
type DrugInteractionCheck struct{ ent.Schema }

func (DrugInteractionCheck) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("prescription_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("order_id", uuid.UUID{}).Optional().Nillable(),
		field.Strings("drug_skus"),
		field.String("result").Default("clear"),
		field.Text("details").Optional(),
		field.UUID("checked_by", uuid.UUID{}).Optional().Nillable(),
		field.Time("checked_at").Default(time.Now),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (DrugInteractionCheck) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id"),
		index.Fields("prescription_id"),
		index.Fields("order_id"),
	}
}
