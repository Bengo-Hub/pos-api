package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Referral holds a loyalty referral: a referrer loyalty account invites a friend by phone, and when
// the referred friend's first qualifying sale finalizes, the referrer is credited bonus points.
type Referral struct{ ent.Schema }

func (Referral) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("referrer_account_id", uuid.UUID{}).Comment("LoyaltyAccount that made the referral"),
		field.String("referred_phone").NotEmpty().Comment("Invited friend's phone (E.164); matched against the sale customer_phone"),
		field.UUID("referred_account_id", uuid.UUID{}).Optional().Nillable().Comment("The referred friend's loyalty account, set when the referral is earned"),
		field.String("code").NotEmpty().Comment("Shareable referral code (unique per tenant)"),
		field.String("status").Default("pending").Comment("pending, earned, expired, cancelled"),
		field.Int("bonus_points").Default(0).Comment("Points credited to the referrer when the referral is earned"),
		field.UUID("earn_transaction_id", uuid.UUID{}).Optional().Nillable().Comment("LoyaltyTransaction that credited the bonus"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("earned_at").Optional().Nillable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (Referral) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "code").Unique(),
		index.Fields("referrer_account_id"),
		index.Fields("tenant_id", "referred_phone"),
		index.Fields("status"),
	}
}
