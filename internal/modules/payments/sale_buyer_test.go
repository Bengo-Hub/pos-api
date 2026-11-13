package payments

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/platform/events"
)

// A linked buyer travels as crm_contact_id alone; an unlinked one keeps name and phone; an
// anonymous sale sends empty fields (S2-8 I-4).
func TestSaleBuyerPayload(t *testing.T) {
	id := uuid.NewString()

	linked := events.WithRecipient(map[string]any{}, "customer", saleBuyer(id, "Asha", "+254712000111"))
	if linked["crm_contact_id"] != id {
		t.Errorf("linked: crm_contact_id = %v", linked["crm_contact_id"])
	}
	for _, k := range []string{"customer_name", "customer_phone", "customer_email"} {
		if _, ok := linked[k]; ok {
			t.Errorf("linked buyer must not carry %s", k)
		}
	}

	unlinked := events.WithRecipient(map[string]any{}, "customer", saleBuyer("", "Asha", "+254712000111"))
	if unlinked["customer_name"] != "Asha" || unlinked["customer_phone"] != "+254712000111" {
		t.Errorf("unlinked: %v", unlinked)
	}
	if _, ok := unlinked["crm_contact_id"]; ok {
		t.Error("unlinked buyer must not carry crm_contact_id")
	}

	if bad := saleBuyer("not-a-uuid", "Asha", "+254712000111"); bad.CrmContactID != nil || bad.Phone == "" {
		t.Errorf("an unparsable id must fall back to the phone, got %+v", bad)
	}
	anon := events.WithRecipient(map[string]any{}, "customer", saleBuyer("", "", ""))
	if anon["customer_phone"] != "" || anon["customer_name"] != "" {
		t.Errorf("anonymous: %v", anon)
	}
}
