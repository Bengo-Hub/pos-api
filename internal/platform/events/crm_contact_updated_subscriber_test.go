package events

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	"github.com/bengobox/pos-service/internal/modules/customers"
)

// Pure Go sqlite registered as "sqlite3" for ent, the same shim other packages use.
type sqlite3Driver struct{ *sqlite.Driver }

func (d sqlite3Driver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	if execer, ok := conn.(interface {
		Exec(string, []driver.Value) (driver.Result, error)
	}); ok {
		if _, err := execer.Exec("PRAGMA foreign_keys = ON;", nil); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func init() { sql.Register("sqlite3", sqlite3Driver{Driver: &sqlite.Driver{}}) }

// newSnapshotTest opens a migrated sqlite database and a fake CRM whose batch endpoint answers
// from contacts.
func newSnapshotTest(t *testing.T, contacts map[uuid.UUID]crm.Contact) (*ent.Client, *CRMContactUpdatedSubscriber) {
	t.Helper()
	db := enttest.Open(t, "sqlite3", filepath.Join(t.TempDir(), "events.sqlite"))
	t.Cleanup(func() { _ = db.Close() })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/internal/contacts/batch" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			IDs []uuid.UUID `json:"ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		out := []crm.Contact{}
		for _, id := range body.IDs {
			if c, ok := contacts[id]; ok {
				out = append(out, c)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	}))
	t.Cleanup(srv.Close)

	res := customers.NewResolver(db, crm.New(srv.URL, "test-key", zap.NewNop()), zap.NewNop())
	return db, NewCRMContactUpdatedSubscriber(res, zap.NewNop())
}

func contactEvent(eventType string, tenantID, contactID uuid.UUID) *sharedevents.Event {
	return sharedevents.NewEvent(eventType, "contact", contactID, tenantID, map[string]any{
		"contact_id": contactID.String(),
		"fields":     []any{"first_name", "last_name", "email", "phone"},
	})
}

func TestContactUpdatedRefreshesSnapshot(t *testing.T) {
	ctx := context.Background()
	tid := uuid.New()
	contactID, clashID := uuid.New(), uuid.New()
	db, sub := newSnapshotTest(t, map[uuid.UUID]crm.Contact{
		contactID: {ID: contactID, FirstName: "Jane", LastName: "Updated", Email: "jane@example.com", Phone: "+254711000999"},
		clashID:   {ID: clashID, FirstName: "Clash", LastName: "Customer", Phone: "+254722000111"},
	})

	acc := db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("0712345678").SetCustomerName("Old Name").
		SetPointsBalance(120).SetCrmContactID(contactID).
		SaveX(ctx)
	rec := db.ClientRecord.Create().
		SetTenantID(tid).SetPhone("0712345678").SetCrmContactID(contactID).
		SaveX(ctx)

	// A second account whose CRM phone is already held by another account must keep its phone.
	holder := db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("+254722000111").SetCustomerName("Phone Holder").
		SaveX(ctx)
	clash := db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("0733000222").SetCustomerName("Clash Old").
		SetCrmContactID(clashID).
		SaveX(ctx)

	if err := sub.handleUpdated(ctx, contactEvent("crm.contact.updated", tid, contactID)); err != nil {
		t.Fatalf("handleUpdated: %v", err)
	}
	if err := sub.handleUpdated(ctx, contactEvent("crm.contact.updated", tid, clashID)); err != nil {
		t.Fatalf("handleUpdated for the clashing contact: %v", err)
	}

	gotAcc := db.LoyaltyAccount.GetX(ctx, acc.ID)
	if gotAcc.CustomerName != "Jane Updated" || gotAcc.CustomerPhone != "+254711000999" || gotAcc.CustomerEmail != "jane@example.com" {
		t.Errorf("account snapshot = %q %q %q, want the CRM's name, phone and email",
			gotAcc.CustomerName, gotAcc.CustomerPhone, gotAcc.CustomerEmail)
	}
	if gotAcc.PointsBalance != 120 {
		t.Errorf("points = %d, want 120 kept", gotAcc.PointsBalance)
	}
	if got := db.ClientRecord.GetX(ctx, rec.ID); got.Phone != "+254711000999" {
		t.Errorf("client phone = %q, want +254711000999", got.Phone)
	}
	gotClash := db.LoyaltyAccount.GetX(ctx, clash.ID)
	if gotClash.CustomerPhone != "0733000222" {
		t.Errorf("clashing account phone = %q, want the old phone kept", gotClash.CustomerPhone)
	}
	if gotClash.CustomerName != "Clash Customer" {
		t.Errorf("clashing account name = %q, want the CRM name still applied", gotClash.CustomerName)
	}
	if got := db.LoyaltyAccount.GetX(ctx, holder.ID); got.CustomerPhone != "+254722000111" {
		t.Errorf("phone holder changed to %q", got.CustomerPhone)
	}
}

// TestContactUpdatedDoesNotRewriteGuestRegister checks a CRM update leaves the guest register
// and booking lead-guest fields as staff recorded them; only the link stays.
func TestContactUpdatedDoesNotRewriteGuestRegister(t *testing.T) {
	ctx := context.Background()
	tid := uuid.New()
	contactID := uuid.New()
	db, sub := newSnapshotTest(t, map[uuid.UUID]crm.Contact{
		contactID: {ID: contactID, FirstName: "Travel", LastName: "Agent", Email: "desk@agency.example", Phone: "+254711000999"},
	})

	room := db.Room.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetRoomNumber("R1").SetName("R1").SetRatePerNight(500).
		SaveX(ctx)
	guest := db.RoomGuest.Create().
		SetTenantID(tid).SetRoomID(room.ID).SetGuestName("Jane Wanjiku").SetFirstName("Jane").SetLastName("Wanjiku").
		SetPhone("+254712345678").SetEmail("jane@example.com").
		SetIDNumber("ID-1").SetCheckInDate(time.Now()).SetNights(1).SetCheckOutDate(time.Now().AddDate(0, 0, 1)).
		SetTotalRoomCharge(500).SetCheckedInBy(uuid.New()).SetCrmContactID(contactID).
		SaveX(ctx)
	booking := db.RoomBooking.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetConfirmationNo("BK-1").SetLeadGuestName("Jane Wanjiku").
		SetPhone("+254712345678").SetEmail("jane@example.com").
		SetArrivalDate(time.Now()).SetDepartureDate(time.Now().AddDate(0, 0, 1)).SetCreatedBy(uuid.New()).
		SetCrmContactID(contactID).
		SaveX(ctx)

	if err := sub.handleUpdated(ctx, contactEvent("crm.contact.updated", tid, contactID)); err != nil {
		t.Fatalf("handleUpdated: %v", err)
	}

	got := db.RoomGuest.GetX(ctx, guest.ID)
	if got.GuestName != "Jane Wanjiku" || got.FirstName != "Jane" || got.LastName != "Wanjiku" ||
		got.Phone != "+254712345678" || got.Email != "jane@example.com" {
		t.Errorf("room guest = %q %q %q %q %q, want the register unchanged",
			got.GuestName, got.FirstName, got.LastName, got.Phone, got.Email)
	}
	if got.CrmContactID == nil || *got.CrmContactID != contactID {
		t.Errorf("room guest link = %v, want %s kept", got.CrmContactID, contactID)
	}
	gotB := db.RoomBooking.GetX(ctx, booking.ID)
	if gotB.LeadGuestName != "Jane Wanjiku" || gotB.Phone != "+254712345678" || gotB.Email != "jane@example.com" {
		t.Errorf("booking = %q %q %q, want the lead guest unchanged", gotB.LeadGuestName, gotB.Phone, gotB.Email)
	}
}

func TestContactDeletedKeepsPoints(t *testing.T) {
	ctx := context.Background()
	tid := uuid.New()
	contactID := uuid.New()
	db, sub := newSnapshotTest(t, nil)

	acc := db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("+254712345678").SetCustomerName("Jane Doe").
		SetPointsBalance(300).SetLifetimePoints(450).SetCrmContactID(contactID).
		SetMetadata(map[string]any{"source": "import"}).
		SaveX(ctx)
	rec := db.ClientRecord.Create().
		SetTenantID(tid).SetPhone("+254712345678").SetNotes("prefers mornings").SetCrmContactID(contactID).
		SaveX(ctx)

	if err := sub.handleDeleted(ctx, contactEvent("crm.contact.deleted", tid, contactID)); err != nil {
		t.Fatalf("handleDeleted: %v", err)
	}

	got, err := db.LoyaltyAccount.Get(ctx, acc.ID)
	if err != nil {
		t.Fatalf("account is gone after a CRM delete: %v", err)
	}
	if got.CrmContactID != nil {
		t.Errorf("crm_contact_id = %v, want cleared", *got.CrmContactID)
	}
	if got.PointsBalance != 300 || got.LifetimePoints != 450 {
		t.Errorf("points = %d / %d, want 300 / 450 kept", got.PointsBalance, got.LifetimePoints)
	}
	if got.CustomerPhone != "+254712345678" || got.CustomerName != "Jane Doe" {
		t.Errorf("snapshot changed on delete: %q %q", got.CustomerPhone, got.CustomerName)
	}
	at, _ := got.Metadata[customers.UnlinkedAtKey].(string)
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Errorf("account metadata %v lacks an RFC3339 %s marker", got.Metadata, customers.UnlinkedAtKey)
	}
	if got.Metadata["source"] != "import" {
		t.Errorf("account metadata = %v, want existing keys kept", got.Metadata)
	}
	gotRec, err := db.ClientRecord.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("client record is gone after a CRM delete: %v", err)
	}
	if gotRec.CrmContactID != nil || gotRec.Notes != "prefers mornings" {
		t.Errorf("client record = link %v notes %q, want link cleared and notes kept", gotRec.CrmContactID, gotRec.Notes)
	}
	if !customers.IsUnlinked(gotRec.Metadata) {
		t.Errorf("client record metadata %v lacks the %s marker", gotRec.Metadata, customers.UnlinkedAtKey)
	}
}
