package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// CustomerBalanceCache is a READ-ONLY, best-effort mirror of a customer's treasury AR balance,
// kept fresh by the durable treasury.customer.balance_updated event consumer (see
// payments/treasury_balance_subscriber.go). It exists ONLY as a self-healing fallback for the
// GetCredit S2S proxy when the live call to treasury fails/times out — treasury remains the
// single source of truth and the live call is always tried first. This closes the one-way sync
// gap where a payment/refund/credit action recorded directly in treasury-ui never reached POS.
type CustomerBalanceCache struct{ ent.Schema }

func (CustomerBalanceCache) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("crm_contact_id", uuid.UUID{}).Optional().Nillable(),
		field.String("customer_identifier").Optional(),
		field.String("customer_name").Optional(),
		field.String("balance_due").Default("0"),
		field.String("outstanding_debit").Default("0"),
		field.String("store_credit_balance").Default("0"),
		field.String("currency").Default("KES"),
		field.Time("synced_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (CustomerBalanceCache) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "crm_contact_id").Unique(),
		index.Fields("tenant_id", "customer_identifier"),
	}
}
