package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/customers"
)

// guestCRM is a marketflow stand-in that keeps every raw upsert body. With status 0 it answers
// with a contact under a fixed id; any other status is returned as a CRM error.
type guestCRM struct {
	mu      sync.Mutex
	bodies  []string
	id      uuid.UUID
	status  int
	refusal string
	// answer, when set, is returned instead of echoing the upsert, so a test can tell whether
	// the CRM's view of a contact leaked back into a local record.
	answer *crm.Contact
}

func newGuestCRM(t *testing.T, status int) (*guestCRM, *httptest.Server) {
	t.Helper()
	g := &guestCRM{id: uuid.New(), status: status, refusal: `{"error":"contact rejected","code":"invalid"}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.bodies = append(g.bodies, string(raw))
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if g.status != 0 {
			w.WriteHeader(g.status)
			_, _ = w.Write([]byte(g.refusal))
			return
		}
		if g.answer != nil {
			_ = json.NewEncoder(w).Encode(g.answer)
			return
		}
		var in map[string]string
		_ = json.Unmarshal(raw, &in)
		_ = json.NewEncoder(w).Encode(crm.Contact{
			ID: g.id, FirstName: in["first_name"], LastName: in["last_name"],
			Email: in["email"], Phone: in["phone"],
		})
	}))
	t.Cleanup(srv.Close)
	return g, srv
}

func (g *guestCRM) calls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.bodies...)
}

// newLinkedHotelHandler is the sqlite hotel harness with a resolver pointed at the fake CRM.
func newLinkedHotelHandler(t *testing.T, crmURL string) (*HotelHandler, *ent.Client) {
	t.Helper()
	h, client := newHotelTestHandler(t)
	h.SetCustomerResolver(customers.NewResolver(client, crm.New(crmURL, "test-key", zap.NewNop()), zap.NewNop()))
	return h, client
}

func seedCheckInRoom(t *testing.T, client *ent.Client, tid uuid.UUID) *ent.Room {
	t.Helper()
	room, err := client.Room.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).
		SetRoomNumber("L1").SetName("L1").SetRatePerNight(800).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed room: %v", err)
	}
	return room
}

// checkInBody carries every guest register field, so the test can prove none reach the CRM.
func checkInBody() map[string]any {
	return map[string]any{
		"guest_name":      "Jane Wanjiku",
		"email":           "jane@example.com",
		"phone":           "0712345678",
		"nationality":     "KE",
		"id_type":         "passport",
		"id_number":       "AK1234567",
		"id_document_url": "ids/guest-scan.jpg",
		"adults":          2,
		"children":        1,
		"child_ages":      []int{7},
		"nights":          2,
	}
}

// TestCheckInLinksGuest checks a check-in without crm_contact_id links the guest through the
// CRM and stores the id, while the guest register fields never reach the CRM.
func TestCheckInLinksGuest(t *testing.T) {
	g, srv := newGuestCRM(t, 0)
	h, client := newLinkedHotelHandler(t, srv.URL)
	tid := uuid.New()
	room := seedCheckInRoom(t, client, tid)

	rec := httptest.NewRecorder()
	h.CheckIn(rec, hotelRoomRequest(t, http.MethodPost, tid, room.ID, checkInBody()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("CheckIn status = %d, body %s", rec.Code, rec.Body.String())
	}

	guest := client.RoomGuest.Query().OnlyX(context.Background())
	if guest.CrmContactID == nil || *guest.CrmContactID != g.id {
		t.Errorf("guest crm_contact_id = %v, want %s", guest.CrmContactID, g.id)
	}
	if guest.Phone != "+254712345678" {
		t.Errorf("guest phone = %q, want +254712345678", guest.Phone)
	}
	if guest.IDNumber != "AK1234567" || guest.Nationality != "KE" {
		t.Errorf("guest register = id %q nationality %q, want them kept locally", guest.IDNumber, guest.Nationality)
	}

	calls := g.calls()
	if len(calls) != 1 {
		t.Fatalf("%d CRM upserts, want 1", len(calls))
	}
	for _, leak := range []string{"id_number", "AK1234567", "nationality", "id_type", "passport", "id_document_url", "guest-scan", "child_ages", "adults"} {
		if strings.Contains(calls[0], leak) {
			t.Errorf("CRM upsert body carries %q: %s", leak, calls[0])
		}
	}
	if !strings.Contains(calls[0], "+254712345678") || !strings.Contains(calls[0], "jane@example.com") {
		t.Errorf("CRM upsert body lacks the guest phone or email: %s", calls[0])
	}
}

// TestCheckInCRMRefusalStillChecksIn checks a CRM refusal never turns a guest away: the
// check-in succeeds and the guest is saved unlinked.
func TestCheckInCRMRefusalStillChecksIn(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusServiceUnavailable} {
		g, srv := newGuestCRM(t, status)
		h, client := newLinkedHotelHandler(t, srv.URL)
		tid := uuid.New()
		room := seedCheckInRoom(t, client, tid)

		rec := httptest.NewRecorder()
		h.CheckIn(rec, hotelRoomRequest(t, http.MethodPost, tid, room.ID, checkInBody()))
		if rec.Code != http.StatusCreated {
			t.Fatalf("CRM %d: CheckIn status = %d, body %s", status, rec.Code, rec.Body.String())
		}
		guest := client.RoomGuest.Query().OnlyX(context.Background())
		if guest.CrmContactID != nil {
			t.Errorf("CRM %d: guest linked to %s, want unlinked", status, *guest.CrmContactID)
		}
		if len(g.calls()) == 0 {
			t.Errorf("CRM %d: the CRM was never asked", status)
		}
	}
}

// TestCheckInFromBookingInheritsContact checks a check-in from a linked booking takes the
// booking's contact without calling the CRM.
func TestCheckInFromBookingInheritsContact(t *testing.T) {
	g, srv := newGuestCRM(t, 0)
	h, client := newLinkedHotelHandler(t, srv.URL)
	tid := uuid.New()
	ctx := context.Background()
	room := seedCheckInRoom(t, client, tid)
	bookingContact := uuid.New()
	booking := client.RoomBooking.Create().
		SetTenantID(tid).SetOutletID(room.OutletID).SetConfirmationNo("BK-LINKED").SetLeadGuestName("Jane Wanjiku").
		SetArrivalDate(time.Now()).SetDepartureDate(time.Now().AddDate(0, 0, 2)).SetCreatedBy(uuid.New()).
		SetCrmContactID(bookingContact).
		SaveX(ctx)

	body := checkInBody()
	body["booking_id"] = booking.ID.String()
	rec := httptest.NewRecorder()
	h.CheckIn(rec, hotelRoomRequest(t, http.MethodPost, tid, room.ID, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("CheckIn status = %d, body %s", rec.Code, rec.Body.String())
	}
	guest := client.RoomGuest.Query().OnlyX(ctx)
	if guest.CrmContactID == nil || *guest.CrmContactID != bookingContact {
		t.Errorf("guest crm_contact_id = %v, want the booking's contact %s", guest.CrmContactID, bookingContact)
	}
	if n := len(g.calls()); n != 0 {
		t.Errorf("%d CRM upserts, want none when the booking already carries the contact", n)
	}
}

// TestRoomBookingLinksLeadGuest checks a booking without crm_contact_id links the lead guest,
// and one that carries an id keeps it without calling the CRM.
func TestRoomBookingLinksLeadGuest(t *testing.T) {
	g, srv := newGuestCRM(t, 0)
	h, client := newLinkedHotelHandler(t, srv.URL)
	tid := uuid.New()
	ctx := context.Background()

	rec := httptest.NewRecorder()
	h.CreateRoomBooking(rec, hotelRoomRequest(t, http.MethodPost, tid, uuid.Nil, map[string]any{
		"lead_guest_name": "Jane Wanjiku", "phone": "0712345678", "email": "jane@example.com",
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("CreateRoomBooking status = %d, body %s", rec.Code, rec.Body.String())
	}
	b := client.RoomBooking.Query().OnlyX(ctx)
	if b.CrmContactID == nil || *b.CrmContactID != g.id {
		t.Errorf("booking crm_contact_id = %v, want %s", b.CrmContactID, g.id)
	}
	if b.Phone != "+254712345678" {
		t.Errorf("booking phone = %q, want +254712345678", b.Phone)
	}

	given := uuid.New()
	rec = httptest.NewRecorder()
	h.CreateRoomBooking(rec, hotelRoomRequest(t, http.MethodPost, tid, uuid.Nil, map[string]any{
		"lead_guest_name": "Known Guest", "phone": "0722000111", "crm_contact_id": given.String(),
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("CreateRoomBooking with id status = %d, body %s", rec.Code, rec.Body.String())
	}
	if n := len(g.calls()); n != 1 {
		t.Errorf("%d CRM upserts, want 1 (a given crm_contact_id must not call the CRM)", n)
	}
	var keptGiven bool
	for _, row := range client.RoomBooking.Query().AllX(ctx) {
		if row.CrmContactID != nil && *row.CrmContactID == given {
			keptGiven = true
		}
	}
	if !keptGiven {
		t.Errorf("the given crm_contact_id %s was not stored", given)
	}
}
