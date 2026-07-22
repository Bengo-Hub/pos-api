package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ControlledSubstanceLog tracks regulated dispensing events for pharmacy outlets.
// Dual-person dispensing register required by pharmacy regulation.
type ControlledSubstanceLog struct {
	ent.Schema
}

func (ControlledSubstanceLog) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("prescription_id", uuid.UUID{}).Optional().Nillable().Comment("FK to Prescription if dispensed under a script"),
		field.UUID("catalog_item_id", uuid.UUID{}).Comment("Controlled substance catalog item"),
		field.String("item_sku").NotEmpty(),
		field.String("item_name").NotEmpty(),
		field.Float("quantity_dispensed"),
		field.UUID("dispensed_by", uuid.UUID{}).Comment("Staff member who dispensed"),
		field.String("patient_name").NotEmpty(),
		field.String("patient_id_number").Optional().Comment("National ID / passport"),
		field.UUID("witness_staff_id", uuid.UUID{}).Optional().Nillable().Comment("Witnessing pharmacist for dual-person requirement"),
		field.String("notes").Optional(),
		// Batch traceability for the regulator export (Phase 9): which lot this controlled
		// dispense was drawn from, populated from POSOrderLine.lot_number/expiry_date once
		// the sale-finalize consumption response reports the FEFO-selected lot.
		field.String("lot_number").Optional(),
		field.Time("lot_expiry_date").Optional().Nillable(),
		field.Time("dispensed_at").Default(time.Now).Immutable(),
	}
}

func (ControlledSubstanceLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id"),
		index.Fields("tenant_id", "catalog_item_id"),
		index.Fields("tenant_id", "dispensed_at"),
	}
}
