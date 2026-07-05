package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// POSOrder holds the schema definition for the POSOrder entity.
type POSOrder struct {
	ent.Schema
}

// Fields of the POSOrder.
func (POSOrder) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.UUID("device_id", uuid.UUID{}),
		field.UUID("user_id", uuid.UUID{}),
		field.String("order_number").
			NotEmpty().
			Unique(),
		// client_reference is the offline client's locally-generated id (uuid). It is the
		// idempotency anchor for offline-created sales: CreateOrder is get-or-create on
		// (tenant_id, client_reference), so a replayed sync returns the existing order
		// instead of creating a duplicate (and re-deducting stock / re-publishing events).
		field.String("client_reference").
			Optional().
			Nillable(),
		// offline_created_at is when the sale was actually rung up on the device (the client
		// clock), distinct from created_at (server ingestion time). Used for receipts/reports
		// so offline sales show the real transaction time.
		field.Time("offline_created_at").
			Optional().
			Nillable(),
		field.String("status").
			Default("draft"),
		// source distinguishes where the sale originated: "pos_terminal" (rung up on the POS
		// terminal) vs "back_office" (entered via the back-office "Add Sale" flow). Drives the
		// All-Sales "Sources" filter and the separate POS-only sales list.
		field.String("source").
			Default("pos_terminal").
			Comment(`Origin of the sale: "pos_terminal" or "back_office"`),
		field.Float("subtotal"),
		field.Float("tax_total"),
		field.Float("discount_total").
			Default(0),
		field.Float("total_amount"),
		field.String("currency").
			Default("KES"),
		field.Enum("order_subtype").
			Values("dine_in", "takeaway", "room_service", "delivery", "bar_tab", "retail").
			Default("dine_in"),
		field.UUID("room_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Room service: linked room"),
		field.UUID("room_guest_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Room service: linked guest stay"),
		field.JSON("metadata", map[string]any{}).
			Default(map[string]any{}),
		// Hospitality: number of covers (guests) at the table, set when waiter opens the table.
		field.Int("covers_count").
			Default(0).
			Comment("Number of covers (guests) at the table"),
		// Hospitality: service charge applied to dine-in orders.
		field.Float("service_charge_percent").
			Default(0).
			Comment("Service charge percentage (e.g. 10.0 for 10%)"),
		field.Float("service_charge_amount").
			Default(0).
			Comment("Computed service charge amount = total_amount * service_charge_percent / 100"),
		// Course firing: tracks the highest course number whose items have been sent to the kitchen.
		// KDS shows items with course_number <= fired_courses; higher courses stay hidden until fired.
		field.Int("fired_courses").
			Default(0).
			Comment("Highest course number sent to KDS. 0 = no courses fired (all course_number=0 items fire immediately)."),
		// Loyalty: customer phone captured at checkout — used by auto-earn on pos.sale.finalized.
		field.String("customer_phone").Optional().Nillable(),
		field.String("customer_name").Optional().Nillable(),
		// eTIMS fields — set by treasury.etims.invoice_transmitted NATS subscriber.
		// treasury-api owns eTIMS submission; pos-api only stores the outcome for receipt generation.
		field.String("etims_invoice_number").
			Optional().
			Nillable(),
		field.String("etims_qr_code_url").
			Optional().
			Nillable(),
		// Receipt reprint tracking — incremented on each explicit reprint so
		// duplicate receipts (a cash-skimming vector) are flagged + audited.
		field.Int("reprint_count").
			Default(0).
			Comment("Number of times the receipt has been explicitly reprinted"),
		// Void fields — set when Admin/Manager voids an order.
		field.String("voided_reason").
			Optional().
			Nillable(),
		field.UUID("voided_by", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Time("voided_at").
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the POSOrder.
func (POSOrder) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("lines", POSOrderLine.Type),
		edge.To("payments", POSPayment.Type),
		edge.To("events", POSOrderEvent.Type),
	}
}

// Indexes of the POSOrder.
func (POSOrder) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "order_number").Unique(),
		// Backstop dedup for offline sales: even if the idempotency-key cache is evicted,
		// a replayed offline order with the same client_reference cannot be inserted twice.
		index.Fields("tenant_id", "client_reference").Unique(),
		// Speeds up the All-Sales "Sources" filter + the POS-only sales list.
		index.Fields("tenant_id", "source"),
	}
}
