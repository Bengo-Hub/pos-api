package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// DocumentSequence holds per-tenant, per-doc-type atomic counters + format config
// for branded document numbering (order, pos_receipt, pos_return, pos_reversal,
// repair_job). Mirrors inventory/treasury-api's DocumentSequence; the service layer
// increments via optimistic CAS. Platform default is PURE NUMERIC (empty prefix +
// empty date_format), tenants opt into a prefixed/dated style in Settings.
type DocumentSequence struct {
	ent.Schema
}

func (DocumentSequence) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).Comment("Tenant this sequence belongs to"),
		field.String("doc_type").NotEmpty().Comment("order, pos_receipt, pos_return, pos_reversal, repair_job, ..."),
		field.String("prefix").Optional().Comment("Number prefix, e.g. POS, RCT"),
		field.String("separator").Default("-"),
		field.String("date_format").Optional().Comment("YYYYMMDD, YYMMDD, MMYY — empty means no date"),
		field.Int("padding").Default(6),
		field.String("reset_freq").Default("never").Comment("daily, monthly, yearly, never"),
		field.Int64("current_val").Default(0),
		field.Time("last_reset").Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (DocumentSequence) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "doc_type").Unique(),
	}
}
