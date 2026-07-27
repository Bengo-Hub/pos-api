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
		// served_by_user_id is WHO IS CREDITED with serving this sale — distinct from the
		// immutable user_id (who created this DB row). They start equal at creation, but
		// served_by_user_id is the one explicitly carried forward when a parked draft is
		// resumed, edited, and finalized as a new order (see orders.Service.CreateOrder's
		// ServedByUserID), so the finalized sale still credits the original drafter instead of
		// silently reassigning to whoever happened to finish it. Nil = fall back to user_id at
		// read time (every pre-existing row). Also the field admins correct via the
		// sale-info edit tool (orders_sale_info_edit.go) when a cashier forgot to log in as
		// themselves.
		field.UUID("served_by_user_id", uuid.UUID{}).
			Optional().
			Nillable(),
		// order_number is unique per-TENANT only (enforced by the composite
		// posorder_tenant_id_order_number index below), never globally — a bare .Unique()
		// here ALSO emits a second, global single-column unique index/constraint
		// (pos_orders_order_number_key), which is wrong: the tenant document sequence
		// (documents.DocTypeOrder) is pure numeric per-tenant by default, so tenant A's and
		// tenant B's "000001" collide on that global constraint the instant more than one
		// tenant places an order. Root cause of the 2026-07-26 "duplicate key value violates
		// unique constraint pos_orders_order_number_key" outage. Do not re-add .Unique() here.
		field.String("order_number").
			NotEmpty(),
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
		// Additional order-level costs (packaging/service/shipping) that increase total_amount;
		// per-charge breakdown lives in metadata.charges.
		field.Float("charges_total").
			Default(0).
			Comment("Sum of additional charges (packaging, service, shipping) included in total_amount"),
		// Ceiling round-off so totals are always whole currency units:
		// total_amount = subtotal + tax_total - discount_total + charges_total + round_off.
		field.Float("round_off").
			Default(0).
			Comment("Amount added to round total_amount up to the next whole number (0 <= round_off < 1)"),
		// Sum of this order's COMPLETED payments, maintained by the payments service
		// (recomputed from pos_payments on every payment mutation). Single source of
		// truth for the paid/partial/due payment-status filter AND the row badge, so
		// the two can never disagree.
		field.Float("paid_total").
			Default(0),
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
		// Full fiscalisation identity for the printed "KRA TIMS Details" block (mirrors a
		// paper ETR receipt): SCU ID (device serial), CU Inv No = {SCU ID}/{receipt no},
		// the KRA receipt signature, and the taxpayer KRA PIN the sale transmitted under.
		field.String("etims_scu_id").
			Optional().
			Nillable().
			Comment("OSCU device serial — the SCU ID line on ETR receipts"),
		field.String("etims_cu_inv_no").
			Optional().
			Nillable().
			Comment("Formatted control-unit invoice number {SCU ID}/{rcptNo}"),
		field.String("etims_rcpt_sign").
			Optional().
			Nillable().
			Comment("KRA receipt signature (rcptSign) — fiscal signing proof"),
		field.String("etims_kra_pin").
			Optional().
			Nillable().
			Comment("Taxpayer KRA PIN the sale was fiscalised under (receipt header line)"),
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
		// business_date is an admin/platform-owner override of which calendar day this
		// order counts toward in reports — e.g. a sale rung up and settled a day late
		// (offline recovery, a missing recipe blocking checkout) that must be reported
		// under the earlier day so records aren't mixed up. Deliberately separate from
		// the immutable created_at (server ingestion time, the real audit timestamp):
		// this field is the only reporting date admins may correct, and every move is
		// logged via date_moved_by/at/reason. Nil = report under created_at as today.
		field.Time("business_date").
			Optional().
			Nillable(),
		field.String("date_moved_reason").
			Optional().
			Nillable(),
		field.UUID("date_moved_by", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Time("date_moved_at").
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
		// public_token is the capability token for the unauthenticated receipt-share link
		// (WhatsApp/Email/SMS "download your receipt" flow) -- mirrors treasury-api's
		// Invoice.PublicToken pattern. The token itself IS the auth: GetPublicReceipt(PDF)
		// resolves an order by this column with no tenant_id in the URL. Auto-generated,
		// never exposed anywhere except inside the durable share link.
		field.UUID("public_token", uuid.UUID{}).
			Default(uuid.New).
			Unique(),
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
