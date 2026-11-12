package handlers

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

// pos.hotel.check_in / check_out for a guest linked to the CRM carry the contact id and the
// stay ids only. The guest register row itself is not touched.
func TestHotelGuestPayloadLinkedHasNoPersonalData(t *testing.T) {
	id := uuid.New()
	guest := &ent.RoomGuest{ID: uuid.New(), CrmContactID: &id, GuestName: "Jane", Email: "jane@example.com", Phone: "0712345678"}

	p := withGuestRecipient(map[string]any{"guest_id": guest.ID, "room_number": "12"}, guest, "Jane")
	if p["crm_contact_id"] != id.String() || p["guest_id"] != guest.ID || p["room_number"] != "12" {
		t.Fatalf("got %v", p)
	}
	for _, k := range []string{"guest_name", "guest_email", "guest_phone"} {
		if _, ok := p[k]; ok {
			t.Errorf("linked guest payload must not carry %q", k)
		}
	}
	if guest.Email != "jane@example.com" || guest.Phone != "0712345678" {
		t.Error("the guest row must not change")
	}
}

// An unlinked guest (CRM down or refused) keeps the details given at the desk.
func TestHotelGuestPayloadUnlinkedKeepsFields(t *testing.T) {
	guest := &ent.RoomGuest{ID: uuid.New(), GuestName: "Jane W", Email: "jane@example.com", Phone: "0712345678"}

	p := withGuestRecipient(map[string]any{"guest_id": guest.ID}, guest, "Jane")
	if p["guest_name"] != "Jane" || p["guest_email"] != "jane@example.com" || p["guest_phone"] != "0712345678" {
		t.Fatalf("got %v, want the guest fields", p)
	}
	if _, ok := p["crm_contact_id"]; ok {
		t.Error("unlinked guest has no crm_contact_id")
	}
}
