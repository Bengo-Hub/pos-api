package events

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

// A person linked to the CRM is named by crm_contact_id alone: notifications-api looks up the
// name, email and phone from the CRM when it sends.
func TestWithRecipientLinkedSendsOnlyTheContactID(t *testing.T) {
	id := uuid.New()
	p := WithRecipient(map[string]any{"account_id": "a1"}, "customer", Recipient{
		CrmContactID: &id, Name: "Jane", Email: "jane@example.com", Phone: "+254712345678",
	})
	if p["crm_contact_id"] != id.String() || p["account_id"] != "a1" {
		t.Fatalf("got %v, want the contact id and the ids", p)
	}
	for _, k := range []string{"customer_name", "customer_email", "customer_phone"} {
		if _, ok := p[k]; ok {
			t.Errorf("linked payload must not carry %q", k)
		}
	}
}

// A person with no CRM link keeps name, email and phone so the notice still reaches them.
func TestWithRecipientUnlinkedKeepsFields(t *testing.T) {
	p := WithRecipient(map[string]any{}, "guest", Recipient{Name: "Ann", Email: "ann@example.com", Phone: "0711111111"})
	if p["guest_name"] != "Ann" || p["guest_email"] != "ann@example.com" || p["guest_phone"] != "0711111111" {
		t.Fatalf("got %v, want the guest fields", p)
	}
	if _, ok := p["crm_contact_id"]; ok {
		t.Error("unlinked payload has no crm_contact_id")
	}

	nilID := uuid.Nil
	p = WithRecipient(map[string]any{}, "customer", Recipient{CrmContactID: &nilID, Phone: "0700"})
	if p["customer_phone"] != "0700" {
		t.Fatalf("a nil contact id counts as unlinked: %v", p)
	}
}

// pos.loyalty.points.earned for a linked account carries the account and contact ids only.
func TestLoyaltyPointsEarnedPayloadLinked(t *testing.T) {
	id := uuid.New()
	acc := &ent.LoyaltyAccount{ID: uuid.New(), CrmContactID: &id, CustomerName: "Jane", CustomerEmail: "jane@example.com", CustomerPhone: "0712345678"}

	p := LoyaltyPointsEarnedPayload(acc, 10, 50)
	if p["crm_contact_id"] != id.String() || p["account_id"] != acc.ID.String() || p["points_earned"] != 10 || p["balance_after"] != 50 {
		t.Fatalf("got %v", p)
	}
	for _, k := range []string{"customer_name", "customer_email", "customer_phone"} {
		if _, ok := p[k]; ok {
			t.Errorf("linked payload must not carry %q", k)
		}
	}
}

func TestLoyaltyPointsEarnedPayloadUnlinked(t *testing.T) {
	acc := &ent.LoyaltyAccount{ID: uuid.New(), CustomerName: "Jane", CustomerEmail: "jane@example.com", CustomerPhone: "0712345678"}

	p := LoyaltyPointsEarnedPayload(acc, 10, 50)
	if p["customer_name"] != "Jane" || p["customer_email"] != "jane@example.com" || p["customer_phone"] != "0712345678" {
		t.Fatalf("got %v, want the customer fields", p)
	}
}
