package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// DailyClosing aggregates all shifts (CashDrawer entries) for an outlet+date into a reconciliation record.
type DailyClosing struct {
	ent.Schema
}

func (DailyClosing) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("outlet_id", uuid.UUID{}),
		field.Time("business_date").
			Comment("Calendar date this closing covers (UTC midnight)"),
		field.Float("total_sales").
			Default(0),
		field.Float("total_refunds").
			Default(0),
		field.Float("total_discounts").
			Default(0),
		field.Float("total_voids").
			Default(0),
		field.Float("cash_expected").
			Default(0).
			Comment("Computed: starting_cash + cash_sales - cash_refunds"),
		field.Float("cash_actual").
			Default(0).
			Comment("Physically counted by manager"),
		field.Float("variance").
			Default(0).
			Comment("cash_actual - cash_expected"),
		field.String("status").
			Default("open").
			Comment("open | closed | reconciled"),
		field.UUID("closed_by", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("User ID of manager who closed the day"),
		field.String("notes").
			Optional(),
		field.JSON("drawer_ids", []uuid.UUID{}).
			Default([]uuid.UUID{}).
			Comment("IDs of CashDrawer rows aggregated into this closing"),
		// Tender breakdown (added Sprint 11)
		field.Float("total_card").Default(0),
		field.Float("total_mpesa").Default(0),
		// Logged cash-drawer movements folded into cash_expected (loss-prevention).
		field.Float("total_pay_ins").Default(0),
		field.Float("total_pay_outs").Default(0),
		field.Float("total_cash_drops").Default(0),
		field.Float("total_tax").Default(0),
		field.Float("total_loyalty_redemptions").Default(0),
		field.Float("total_room_charge").Default(0),
		// Order and item counts
		field.Int("total_orders").Default(0),
		field.Int("total_items_sold").Default(0),
		field.Time("closed_at").Optional().Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

func (DailyClosing) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("outlet", Outlet.Type).
			Ref("daily_closings").
			Field("outlet_id").
			Unique().
			Required(),
	}
}

func (DailyClosing) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "outlet_id", "business_date").Unique(),
	}
}
