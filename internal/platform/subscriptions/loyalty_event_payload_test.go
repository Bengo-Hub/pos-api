package subscriptions

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

func noPersonalData(t *testing.T, p map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := p[k]; ok {
			t.Errorf("payload must not carry %q: %v", k, p)
		}
	}
}

// pos.loyalty.tier_upgraded for a linked account: contact id and ids only.
func TestTierUpgradedPayloadLinked(t *testing.T) {
	id := uuid.New()
	acc := &ent.LoyaltyAccount{ID: uuid.New(), CrmContactID: &id, CustomerName: "Jane", CustomerEmail: "jane@example.com", CustomerPhone: "0712345678"}
	prog := uuid.New()

	p := tierUpgradedPayload(acc, prog, "member", "gold", 1200)
	if p["crm_contact_id"] != id.String() || p["account_id"] != acc.ID.String() || p["new_tier"] != "gold" || p["program_id"] != prog.String() {
		t.Fatalf("got %v", p)
	}
	noPersonalData(t, p, "customer_name", "customer_email", "customer_phone")
}

// An unlinked account keeps the customer fields; marketflow still finds the contact by phone.
func TestTierUpgradedPayloadUnlinked(t *testing.T) {
	acc := &ent.LoyaltyAccount{ID: uuid.New(), CustomerName: "Jane", CustomerEmail: "jane@example.com", CustomerPhone: "0712345678"}

	p := tierUpgradedPayload(acc, uuid.New(), "member", "gold", 1200)
	if p["customer_phone"] != "0712345678" || p["customer_name"] != "Jane" || p["customer_email"] != "jane@example.com" {
		t.Fatalf("got %v, want the customer fields", p)
	}
	if _, ok := p["crm_contact_id"]; ok {
		t.Error("unlinked payload has no crm_contact_id")
	}
}

// The referral notice goes to the referrer, so crm_contact_id is the REFERRER's contact even
// when the referred friend is linked to a different one. Neither person's details travel.
func TestReferralEarnedPayloadUsesReferrerContact(t *testing.T) {
	referrerID, referredID := uuid.New(), uuid.New()
	referrer := &ent.LoyaltyAccount{ID: uuid.New(), CrmContactID: &referrerID, CustomerName: "Ann", CustomerEmail: "ann@example.com", CustomerPhone: "0711111111"}
	referred := &ent.LoyaltyAccount{ID: uuid.New(), CrmContactID: &referredID, CustomerPhone: "0722222222"}
	ref := &ent.Referral{ID: uuid.New(), BonusPoints: 20}

	p := referralEarnedPayload(ref, referrer, referred, "0722222222", 70)
	if p["crm_contact_id"] != referrerID.String() {
		t.Fatalf("crm_contact_id = %v, want the referrer's %s", p["crm_contact_id"], referrerID)
	}
	if p["referred_crm_contact_id"] != referredID.String() || p["referrer_account"] != referrer.ID.String() || p["bonus_points"] != 20 || p["balance_after"] != 70 {
		t.Fatalf("got %v", p)
	}
	noPersonalData(t, p, "referrer_name", "referrer_email", "referrer_phone", "referred_phone")
}

// Unlinked referrer and friend: the fields stay so the referrer is still thanked.
func TestReferralEarnedPayloadUnlinkedKeepsFields(t *testing.T) {
	referrer := &ent.LoyaltyAccount{ID: uuid.New(), CustomerName: "Ann", CustomerEmail: "ann@example.com", CustomerPhone: "0711111111"}
	ref := &ent.Referral{ID: uuid.New(), BonusPoints: 20}

	p := referralEarnedPayload(ref, referrer, nil, "0722222222", 70)
	if p["referrer_name"] != "Ann" || p["referrer_email"] != "ann@example.com" || p["referrer_phone"] != "0711111111" || p["referred_phone"] != "0722222222" {
		t.Fatalf("got %v, want the referrer fields and the friend's phone", p)
	}
	noPersonalData(t, p, "crm_contact_id", "referred_crm_contact_id")
}
