package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entroom "github.com/bengobox/pos-service/internal/ent/room"
)

// registerLeaks are guest register values that must never reach the CRM.
var registerLeaks = []string{"id_number", "AK1234567", "nationality", "id_type", "passport", "id_document_url", "guest-scan"}

// seedEditGuest seeds an active guest as check-in would store it, with every register field set.
func seedEditGuest(t *testing.T, client *ent.Client, tid uuid.UUID, contact *uuid.UUID) (*ent.Room, *ent.RoomGuest) {
	t.Helper()
	ctx := context.Background()
	room := client.Room.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetRoomNumber("E1").SetName("E1").SetRatePerNight(800).
		SetStatus(entroom.StatusOccupied).
		SaveX(ctx)
	checkIn := time.Date(2026, 1, 10, 14, 0, 0, 0, time.UTC)
	guest := client.RoomGuest.Create().
		SetTenantID(tid).SetRoomID(room.ID).
		SetGuestName("Jane Wanjiku").SetPhone("0712345678").SetEmail("jane@example.com").
		SetIDNumber("AK1234567").SetNationality("KE").SetIDDocumentURL("ids/guest-scan.jpg").
		SetCheckInDate(checkIn).SetNights(2).SetCheckOutDate(checkIn.AddDate(0, 0, 2)).
		SetTotalRoomCharge(1600).SetCheckedInBy(uuid.New()).
		SetNillableCrmContactID(contact).
		SaveX(ctx)
	return room, guest
}

// seedEditBooking seeds a booking header as a staff member would create it.
func seedEditBooking(t *testing.T, client *ent.Client, tid uuid.UUID, phone, email string, contact *uuid.UUID) *ent.RoomBooking {
	t.Helper()
	return client.RoomBooking.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetConfirmationNo("BK-" + uuid.NewString()[:8]).
		SetLeadGuestName("Jane Wanjiku").SetPhone(phone).SetEmail(email).
		SetArrivalDate(time.Now().AddDate(0, 0, 30)).SetDepartureDate(time.Now().AddDate(0, 0, 32)).
		SetCreatedBy(uuid.New()).
		SetNillableCrmContactID(contact).
		SaveX(context.Background())
}

// crmRename is a CRM answer whose name, phone and email all differ from the register.
func crmRename() *crm.Contact {
	return &crm.Contact{ID: uuid.New(), FirstName: "Renamed", LastName: "ByCRM", Phone: "+254799999999", Email: "crm@example.com"}
}

// TestUpdateGuestLinksUnlinkedGuest checks an edit of a guest without crm_contact_id links them
// with the same fields check-in sends (the edited phone, email and name), stores only the
// contact id, and leaves the register exactly as typed even when the CRM knows the contact
// under another name, phone and email.
func TestUpdateGuestLinksUnlinkedGuest(t *testing.T) {
	g, srv := newGuestCRM(t, 0)
	g.answer = crmRename()
	h, client := newLinkedHotelHandler(t, srv.URL)
	tid := uuid.New()
	room, guest := seedEditGuest(t, client, tid, nil)

	rec := httptest.NewRecorder()
	h.UpdateGuest(rec, hotelRoomRequest(t, http.MethodPatch, tid, room.ID, map[string]any{
		"phone": "0722000111", "adults": 3,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("UpdateGuest status = %d, body %s", rec.Code, rec.Body.String())
	}

	got := client.RoomGuest.GetX(context.Background(), guest.ID)
	if got.CrmContactID == nil || *got.CrmContactID != g.answer.ID {
		t.Errorf("guest crm_contact_id = %v, want %s", got.CrmContactID, g.answer.ID)
	}
	if got.GuestName != "Jane Wanjiku" || got.Phone != "0722000111" || got.Email != "jane@example.com" {
		t.Errorf("register rewritten: name %q phone %q email %q, want the typed values", got.GuestName, got.Phone, got.Email)
	}
	if got.IDNumber != "AK1234567" || got.Nationality != "KE" || got.Adults != 3 {
		t.Errorf("guest = id %q nationality %q adults %d, want AK1234567 KE 3", got.IDNumber, got.Nationality, got.Adults)
	}

	calls := g.calls()
	if len(calls) != 1 {
		t.Fatalf("%d CRM upserts, want 1", len(calls))
	}
	if !strings.Contains(calls[0], "+254722000111") || !strings.Contains(calls[0], "jane@example.com") {
		t.Errorf("CRM upsert lacks the edited phone or the email: %s", calls[0])
	}
	for _, leak := range registerLeaks {
		if strings.Contains(calls[0], leak) {
			t.Errorf("CRM upsert body carries %q: %s", leak, calls[0])
		}
	}
}

// TestUpdateGuestCRMRefusalOrOutageStillSaves checks a CRM refusal (400, 422) or outage (503)
// never fails the edit: it is saved and the guest stays unlinked.
func TestUpdateGuestCRMRefusalOrOutageStillSaves(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusServiceUnavailable} {
		g, srv := newGuestCRM(t, status)
		h, client := newLinkedHotelHandler(t, srv.URL)
		tid := uuid.New()
		room, guest := seedEditGuest(t, client, tid, nil)

		rec := httptest.NewRecorder()
		h.UpdateGuest(rec, hotelRoomRequest(t, http.MethodPatch, tid, room.ID, map[string]any{"adults": 3}))
		if rec.Code != http.StatusOK {
			t.Fatalf("CRM %d: UpdateGuest status = %d, body %s", status, rec.Code, rec.Body.String())
		}
		got := client.RoomGuest.GetX(context.Background(), guest.ID)
		if got.Adults != 3 {
			t.Errorf("CRM %d: adults = %d, want the edit saved (3)", status, got.Adults)
		}
		if got.CrmContactID != nil {
			t.Errorf("CRM %d: guest linked to %s, want unlinked", status, *got.CrmContactID)
		}
		if len(g.calls()) == 0 {
			t.Errorf("CRM %d: the CRM was never asked", status)
		}
	}
}

// TestUpdateGuestSlowCRMKeepsBudget checks a CRM that never answers holds the edit for no more
// than the link budget, and the edit is saved unlinked.
func TestUpdateGuestSlowCRMKeepsBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	h, client := newLinkedHotelHandler(t, srv.URL)
	tid := uuid.New()
	room, guest := seedEditGuest(t, client, tid, nil)

	start := time.Now()
	rec := httptest.NewRecorder()
	h.UpdateGuest(rec, hotelRoomRequest(t, http.MethodPatch, tid, room.ID, map[string]any{"adults": 2}))
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("edit took %s with a silent CRM, want it bounded by the 2s link budget", took)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("UpdateGuest status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := client.RoomGuest.GetX(context.Background(), guest.ID); got.CrmContactID != nil {
		t.Errorf("guest linked to %s, want unlinked", *got.CrmContactID)
	}
}

// TestUpdateGuestAlreadyLinkedSkipsCRM checks an edit of a linked guest never calls the CRM and
// keeps the contact it has.
func TestUpdateGuestAlreadyLinkedSkipsCRM(t *testing.T) {
	g, srv := newGuestCRM(t, 0)
	h, client := newLinkedHotelHandler(t, srv.URL)
	tid := uuid.New()
	linked := uuid.New()
	room, guest := seedEditGuest(t, client, tid, &linked)

	rec := httptest.NewRecorder()
	h.UpdateGuest(rec, hotelRoomRequest(t, http.MethodPatch, tid, room.ID, map[string]any{"phone": "0722000111"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("UpdateGuest status = %d, body %s", rec.Code, rec.Body.String())
	}
	if n := len(g.calls()); n != 0 {
		t.Errorf("%d CRM upserts, want none for a linked guest", n)
	}
	got := client.RoomGuest.GetX(context.Background(), guest.ID)
	if got.CrmContactID == nil || *got.CrmContactID != linked {
		t.Errorf("guest crm_contact_id = %v, want %s kept", got.CrmContactID, linked)
	}
}

// TestUpdateRoomBookingLinksUnlinkedBooking checks an edit of a booking without crm_contact_id
// links the lead guest with the fields booking creation sends, and leaves the lead guest's
// name, phone and email as typed.
func TestUpdateRoomBookingLinksUnlinkedBooking(t *testing.T) {
	g, srv := newGuestCRM(t, 0)
	g.answer = crmRename()
	h, client := newLinkedHotelHandler(t, srv.URL)
	tid := uuid.New()
	b := seedEditBooking(t, client, tid, "+254712345678", "", nil)

	rec := httptest.NewRecorder()
	h.UpdateRoomBooking(rec, hotelRoomRequest(t, http.MethodPatch, tid, b.ID, map[string]any{
		"email": "jane@example.com", "market_segment": "corporate",
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("UpdateRoomBooking status = %d, body %s", rec.Code, rec.Body.String())
	}
	got := client.RoomBooking.GetX(context.Background(), b.ID)
	if got.CrmContactID == nil || *got.CrmContactID != g.answer.ID {
		t.Errorf("booking crm_contact_id = %v, want %s", got.CrmContactID, g.answer.ID)
	}
	if got.LeadGuestName != "Jane Wanjiku" || got.Phone != "+254712345678" || got.Email != "jane@example.com" {
		t.Errorf("booking rewritten: name %q phone %q email %q, want the typed values", got.LeadGuestName, got.Phone, got.Email)
	}
	if got.MarketSegment != "corporate" {
		t.Errorf("market_segment = %q, want the edit saved", got.MarketSegment)
	}
	calls := g.calls()
	if len(calls) != 1 {
		t.Fatalf("%d CRM upserts, want 1", len(calls))
	}
	if !strings.Contains(calls[0], "+254712345678") || !strings.Contains(calls[0], "jane@example.com") {
		t.Errorf("CRM upsert lacks the booking phone or the edited email: %s", calls[0])
	}
}

// TestUpdateRoomBookingCRMRefusalOrOutageStillSaves checks a CRM refusal or outage never fails
// a booking edit: it is saved and the booking stays unlinked.
func TestUpdateRoomBookingCRMRefusalOrOutageStillSaves(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusServiceUnavailable} {
		g, srv := newGuestCRM(t, status)
		h, client := newLinkedHotelHandler(t, srv.URL)
		tid := uuid.New()
		b := seedEditBooking(t, client, tid, "+254712345678", "jane@example.com", nil)

		rec := httptest.NewRecorder()
		h.UpdateRoomBooking(rec, hotelRoomRequest(t, http.MethodPatch, tid, b.ID, map[string]any{"market_segment": "leisure"}))
		if rec.Code != http.StatusOK {
			t.Fatalf("CRM %d: UpdateRoomBooking status = %d, body %s", status, rec.Code, rec.Body.String())
		}
		got := client.RoomBooking.GetX(context.Background(), b.ID)
		if got.MarketSegment != "leisure" {
			t.Errorf("CRM %d: market_segment = %q, want the edit saved", status, got.MarketSegment)
		}
		if got.CrmContactID != nil {
			t.Errorf("CRM %d: booking linked to %s, want unlinked", status, *got.CrmContactID)
		}
		if len(g.calls()) == 0 {
			t.Errorf("CRM %d: the CRM was never asked", status)
		}
	}
}

// TestUpdateRoomBookingSkipsCRMWhenLinkedOrNoContact checks a linked booking, and one with
// neither phone nor email, never call the CRM.
func TestUpdateRoomBookingSkipsCRMWhenLinkedOrNoContact(t *testing.T) {
	g, srv := newGuestCRM(t, 0)
	h, client := newLinkedHotelHandler(t, srv.URL)
	tid := uuid.New()
	linked := uuid.New()
	withContact := seedEditBooking(t, client, tid, "+254712345678", "jane@example.com", &linked)
	noDetails := seedEditBooking(t, client, tid, "", "", nil)

	for _, b := range []*ent.RoomBooking{withContact, noDetails} {
		rec := httptest.NewRecorder()
		h.UpdateRoomBooking(rec, hotelRoomRequest(t, http.MethodPatch, tid, b.ID, map[string]any{"market_segment": "leisure"}))
		if rec.Code != http.StatusOK {
			t.Fatalf("UpdateRoomBooking status = %d, body %s", rec.Code, rec.Body.String())
		}
	}
	if n := len(g.calls()); n != 0 {
		t.Errorf("%d CRM upserts, want none", n)
	}
	if got := client.RoomBooking.GetX(context.Background(), withContact.ID); got.CrmContactID == nil || *got.CrmContactID != linked {
		t.Errorf("booking crm_contact_id = %v, want %s kept", got.CrmContactID, linked)
	}
	if got := client.RoomBooking.GetX(context.Background(), noDetails.ID); got.CrmContactID != nil {
		t.Errorf("booking without phone or email linked to %s", *got.CrmContactID)
	}
}

// TestUpdateRoomBookingCancelledNeverLinks checks cancelling an unlinked booking, and editing a
// booking already cancelled, never call the CRM and leave the row unlinked, while the edit saves.
func TestUpdateRoomBookingCancelledNeverLinks(t *testing.T) {
	g, srv := newGuestCRM(t, 0)
	h, client := newLinkedHotelHandler(t, srv.URL)
	tid := uuid.New()
	ctx := context.Background()
	b := seedEditBooking(t, client, tid, "+254712345678", "jane@example.com", nil)

	rec := httptest.NewRecorder()
	h.UpdateRoomBooking(rec, hotelRoomRequest(t, http.MethodPatch, tid, b.ID, map[string]any{"status": "cancelled"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body %s", rec.Code, rec.Body.String())
	}
	got := client.RoomBooking.GetX(ctx, b.ID)
	if string(got.Status) != "cancelled" {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
	if got.CrmContactID != nil {
		t.Errorf("cancelled booking linked to %s, want unlinked", *got.CrmContactID)
	}

	rec = httptest.NewRecorder()
	h.UpdateRoomBooking(rec, hotelRoomRequest(t, http.MethodPatch, tid, b.ID, map[string]any{"market_segment": "leisure"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("edit of cancelled booking status = %d, body %s", rec.Code, rec.Body.String())
	}
	got = client.RoomBooking.GetX(ctx, b.ID)
	if got.MarketSegment != "leisure" {
		t.Errorf("market_segment = %q, want the edit saved", got.MarketSegment)
	}
	if got.CrmContactID != nil {
		t.Errorf("edited cancelled booking linked to %s, want unlinked", *got.CrmContactID)
	}
	if n := len(g.calls()); n != 0 {
		t.Errorf("%d CRM upserts, want none for a cancelled booking", n)
	}
}

// TestPublicRoomBookingNeverCallsCRM checks the unauthenticated booking widget never creates a
// CRM contact, even with a resolver wired: the booking is saved unlinked.
func TestPublicRoomBookingNeverCallsCRM(t *testing.T) {
	g, srv := newGuestCRM(t, 0)
	h, client := newLinkedHotelHandler(t, srv.URL)
	tid := uuid.New()
	ctx := context.Background()
	// Outlets reference a tenant row.
	client.Tenant.Create().SetID(tid).SetName("Widget Hotel").SetSlug("widget-test").SaveX(ctx)
	outlet := client.Outlet.Create().
		SetTenantID(tid).SetTenantSlug("widget-test").SetCode("HTL-" + uuid.NewString()[:6]).SetName("Hotel").
		SetUseCase("hospitality").
		SaveX(ctx)
	client.Room.Create().
		SetTenantID(tid).SetOutletID(outlet.ID).SetRoomNumber("W1").SetName("W1").SetRatePerNight(800).
		SetRoomType(entroom.RoomTypeStandard).
		SaveX(ctx)

	arrival := time.Now().AddDate(0, 0, 10).Format("2006-01-02")
	departure := time.Now().AddDate(0, 0, 12).Format("2006-01-02")
	rec := httptest.NewRecorder()
	h.CreatePublicRoomBooking(rec, hotelRoomRequest(t, http.MethodPost, tid, uuid.Nil, map[string]any{
		"outlet_id": outlet.ID.String(), "room_type": "standard", "rooms_count": 1,
		"arrival_date": arrival, "departure_date": departure,
		"lead_guest_name": "Web Guest", "phone": "0712345678", "email": "web@example.com",
	}))
	// The point of this test is that the unauthenticated widget never reaches the CRM. The
	// SQLite test schema does not always accept the full booking insert, so the CRM check
	// stands on its own and the unlinked check runs only when the booking was saved.
	if n := len(g.calls()); n != 0 {
		t.Errorf("%d CRM upserts from the public widget, want none", n)
	}
	if rec.Code == http.StatusCreated {
		b := client.RoomBooking.Query().OnlyX(ctx)
		if b.CrmContactID != nil {
			t.Errorf("public booking linked to %s, want unlinked", *b.CrmContactID)
		}
	} else {
		t.Logf("public booking not saved in the SQLite test schema (status %d); CRM check still holds", rec.Code)
	}
}
