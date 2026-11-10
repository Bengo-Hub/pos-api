package customers

import (
	"context"
	"encoding/json"
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
)

// fakeCRM is a marketflow stand-in that records upsert bodies and answers with a contact
// whose id is stable per phone.
type fakeCRM struct {
	mu      sync.Mutex
	upserts []map[string]string
	ids     map[string]uuid.UUID
}

func newFakeCRM(t *testing.T) (*fakeCRM, *httptest.Server) {
	t.Helper()
	f := &fakeCRM{ids: map[string]uuid.UUID{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/internal/contacts/upsert" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.upserts = append(f.upserts, body)
		id, ok := f.ids[body["phone"]]
		if !ok {
			id = uuid.New()
			f.ids[body["phone"]] = id
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(crm.Contact{
			ID:        id,
			FirstName: body["first_name"],
			LastName:  body["last_name"],
			Email:     body["email"],
			Phone:     body["phone"],
		})
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeCRM) calls() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]string(nil), f.upserts...)
}

func (f *fakeCRM) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts = nil
}

func newTestResolver(t *testing.T, url string) *Resolver {
	t.Helper()
	return NewResolver(openTestDB(t), crm.New(url, "test-key", zap.NewNop()), zap.NewNop())
}

func TestLinkNormalisesAndUpserts(t *testing.T) {
	f, srv := newFakeCRM(t)
	r := newTestResolver(t, srv.URL)
	tid := uuid.New()

	var firstID uuid.UUID
	for _, spelling := range []string{"0712345678", "+254 712 345 678", "254712345678"} {
		f.reset()
		ident, err := r.Link(context.Background(), tid, spelling, "", "Jane Wanjiru Doe")
		if err != nil {
			t.Fatalf("Link(%q): %v", spelling, err)
		}
		calls := f.calls()
		if len(calls) != 1 {
			t.Fatalf("Link(%q) made %d upsert calls, want 1", spelling, len(calls))
		}
		if got := calls[0]["phone"]; got != "+254712345678" {
			t.Errorf("Link(%q) sent phone %q, want +254712345678", spelling, got)
		}
		if calls[0]["first_name"] != "Jane" || calls[0]["last_name"] != "Wanjiru Doe" {
			t.Errorf("Link(%q) sent name %q %q", spelling, calls[0]["first_name"], calls[0]["last_name"])
		}
		if ident.Phone != "+254712345678" {
			t.Errorf("Identity.Phone = %q, want +254712345678", ident.Phone)
		}
		if ident.CrmContactID == nil {
			t.Fatalf("Link(%q) returned no contact id", spelling)
		}
		if firstID == uuid.Nil {
			firstID = *ident.CrmContactID
		} else if *ident.CrmContactID != firstID {
			t.Errorf("Link(%q) got contact %s, want the same contact %s", spelling, *ident.CrmContactID, firstID)
		}
	}
}

func TestLinkCRMDownStillCreates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	r := newTestResolver(t, srv.URL)

	start := time.Now()
	ident, err := r.Link(context.Background(), uuid.New(), "0712345678", "", "Jane Doe")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Link returned %v, want no error while the CRM is down", err)
	}
	// The budget is 2s. The bound leaves room for a loaded test machine but still fails if Link
	// waits for the server's 5s answer.
	if elapsed > 3500*time.Millisecond {
		t.Errorf("Link took %v, want well under the server's 5s stall (budget 2s)", elapsed)
	}
	if ident.CrmContactID != nil {
		t.Errorf("CrmContactID = %v, want nil while the CRM is down", *ident.CrmContactID)
	}
	if ident.Phone != "+254712345678" || ident.Name != "Jane Doe" {
		t.Errorf("Identity = %+v, want the local normalised identity", ident)
	}
}

func TestLinkRejectsInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"phone number is not valid","code":"invalid_phone"}`))
	}))
	t.Cleanup(srv.Close)
	r := newTestResolver(t, srv.URL)

	ident, err := r.Link(context.Background(), uuid.New(), "12", "", "Jane Doe")
	if err == nil {
		t.Fatal("Link returned no error for a CRM 400")
	}
	if !strings.Contains(err.Error(), "phone number is not valid") {
		t.Errorf("error %q does not carry the CRM message", err.Error())
	}
	msg, rejected := RejectionMessage(err)
	if !rejected || msg != "phone number is not valid" {
		t.Errorf("RejectionMessage = %q, %v; want the CRM message and true", msg, rejected)
	}
	if ident.CrmContactID != nil {
		t.Error("a rejected link must not carry a contact id")
	}
}

func TestFindAccountSuffix(t *testing.T) {
	r := NewResolver(openTestDB(t), nil, zap.NewNop())
	ctx := context.Background()
	tid, otherTenant := uuid.New(), uuid.New()

	acc, err := r.db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("0712345678").SetCustomerName("Stored Nationally").
		Save(ctx)
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := r.db.LoyaltyAccount.Create().
		SetTenantID(otherTenant).SetCustomerPhone("+254799999999").SetCustomerName("Other Tenant").
		Save(ctx); err != nil {
		t.Fatalf("seed other account: %v", err)
	}

	for _, spelling := range []string{"+254712345678", "+254 712 345 678", "254712345678", "0712345678"} {
		got, err := r.FindAccount(ctx, tid, spelling)
		if err != nil {
			t.Fatalf("FindAccount(%q): %v", spelling, err)
		}
		if got == nil || got.ID != acc.ID {
			t.Errorf("FindAccount(%q) = %v, want account %s", spelling, got, acc.ID)
		}
	}

	if got, err := r.FindAccount(ctx, otherTenant, "0712345678"); err != nil || got != nil {
		t.Errorf("FindAccount in another tenant = %v, %v; want nil, nil", got, err)
	}
	if got, err := r.FindAccount(ctx, tid, "0700000001"); err != nil || got != nil {
		t.Errorf("FindAccount for an unknown phone = %v, %v; want nil, nil", got, err)
	}
}

func TestLinkAuthFailureSavesUnlinked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key","code":"unauthorized"}`))
	}))
	t.Cleanup(srv.Close)
	r := newTestResolver(t, srv.URL)

	ident, err := r.Link(context.Background(), uuid.New(), "0712345678", "", "Jane Doe")
	if err != nil {
		t.Fatalf("Link returned %v, want no error for a refused service key", err)
	}
	if ident.CrmContactID != nil {
		t.Errorf("CrmContactID = %v, want nil", *ident.CrmContactID)
	}
	if ident.Phone != "+254712345678" {
		t.Errorf("Identity.Phone = %q, want the local normalised phone", ident.Phone)
	}
}

func TestFindAccountForeignNumberNoMatch(t *testing.T) {
	r := NewResolver(openTestDB(t), nil, zap.NewNop())
	ctx := context.Background()
	tid := uuid.New()
	if _, err := r.db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("0712345678").SetCustomerName("Kenyan Customer").
		Save(ctx); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	got, err := r.FindAccount(ctx, tid, "+256712345678")
	if err != nil {
		t.Fatalf("FindAccount: %v", err)
	}
	if got != nil {
		t.Errorf("a Ugandan number matched Kenyan account %s", got.ID)
	}
}

// seedLegacy stores one loyalty account under a given phone spelling.
func seedLegacy(t *testing.T, r *Resolver, tid uuid.UUID, phone string, lifetime int, at time.Time, contactID *uuid.UUID) uuid.UUID {
	t.Helper()
	acc, err := r.db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone(phone).SetCustomerName("Same Number").
		SetPointsBalance(lifetime).SetLifetimePoints(lifetime).SetCreatedAt(at).
		SetNillableCrmContactID(contactID).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed account %q: %v", phone, err)
	}
	return acc.ID
}

func TestFindAccountAmbiguousPicksMostUsed(t *testing.T) {
	r := NewResolver(openTestDB(t), nil, zap.NewNop())
	ctx := context.Background()
	t0 := time.Date(2025, 1, 1, 9, 0, 0, 0, time.UTC)

	// Only legacy spellings are stored and none equals the typed or normalised input, so the
	// suffix step runs and the account with more lifetime points wins.
	tid := uuid.New()
	seedLegacy(t, r, tid, "0712345678", 50, t0, nil)
	busy := seedLegacy(t, r, tid, "254712345678", 200, t0.Add(time.Hour), nil)
	for _, spelling := range []string{"+254712345678", "+254 712 345 678", "+254-712-345-678"} {
		got, err := r.FindAccount(ctx, tid, spelling)
		if err != nil {
			t.Fatalf("FindAccount(%q): %v", spelling, err)
		}
		if got == nil || got.ID != busy {
			t.Errorf("FindAccount(%q) = %v, want the account with more points %s", spelling, got, busy)
		}
	}

	// Equal points: the oldest wins.
	tie := uuid.New()
	seedLegacy(t, r, tie, "0712345678", 100, t0.Add(time.Hour), nil)
	oldest := seedLegacy(t, r, tie, "254712345678", 100, t0, nil)
	got, err := r.FindAccount(ctx, tie, "+254712345678")
	if err != nil {
		t.Fatalf("FindAccount: %v", err)
	}
	if got == nil || got.ID != oldest {
		t.Errorf("FindAccount on a points tie = %v, want the oldest account %s", got, oldest)
	}
}

func TestFindAccountAllSpellingsResolveToOneAccount(t *testing.T) {
	r := NewResolver(openTestDB(t), nil, zap.NewNop())
	ctx := context.Background()
	t0 := time.Date(2025, 1, 1, 9, 0, 0, 0, time.UTC)
	spellings := []string{"+254712345678", "0712345678", "254712345678", "712345678", "+254 712 345 678"}

	// Nothing linked: an empty E.164 row made by old S2S code must not strand the till's
	// legacy balance. Every spelling, the E.164 one included, finds the busiest row.
	tid := uuid.New()
	seedLegacy(t, r, tid, "+254712345678", 0, t0.Add(2*time.Hour), nil)
	busy := seedLegacy(t, r, tid, "0712345678", 500, t0, nil)
	seedLegacy(t, r, tid, "254712345678", 20, t0.Add(time.Hour), nil)
	for _, s := range spellings {
		got, err := r.FindAccount(ctx, tid, s)
		if err != nil {
			t.Fatalf("FindAccount(%q): %v", s, err)
		}
		if got == nil || got.ID != busy {
			t.Errorf("FindAccount(%q) = %v, want the busiest account %s", s, got, busy)
		}
	}

	// A linked row with fewer points does not take the balance away from the busy row; on equal
	// points the linked row wins over an older unlinked one, for every spelling.
	linkedTid := uuid.New()
	contactID := uuid.New()
	seedLegacy(t, r, linkedTid, "+254712345678", 0, t0.Add(2*time.Hour), &contactID)
	busyLegacy := seedLegacy(t, r, linkedTid, "0712345678", 500, t0, nil)
	for _, s := range spellings {
		got, err := r.FindAccount(ctx, linkedTid, s)
		if err != nil {
			t.Fatalf("FindAccount(%q): %v", s, err)
		}
		if got == nil || got.ID != busyLegacy {
			t.Errorf("FindAccount(%q) = %v, want the busiest account %s over the linked twin", s, got, busyLegacy)
		}
	}

	tieTid := uuid.New()
	seedLegacy(t, r, tieTid, "0712345678", 500, t0, nil)
	linked := seedLegacy(t, r, tieTid, "254712345678", 500, t0.Add(time.Hour), &contactID)
	for _, s := range spellings {
		got, err := r.FindAccount(ctx, tieTid, s)
		if err != nil {
			t.Fatalf("FindAccount(%q): %v", s, err)
		}
		if got == nil || got.ID != linked {
			t.Errorf("FindAccount(%q) on a points tie = %v, want the linked account %s", s, got, linked)
		}
	}
}

func TestPickPrefersBusiestThenLinked(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 9, 0, 0, 0, time.UTC)
	tid := uuid.New()
	contactID := uuid.New()
	nilLink := uuid.Nil
	busy := &ent.LoyaltyAccount{ID: uuid.New(), LifetimePoints: 500, CreatedAt: t0.Add(time.Hour)}
	linked := &ent.LoyaltyAccount{ID: uuid.New(), LifetimePoints: 10, CreatedAt: t0, CrmContactID: &contactID}

	// Points first, in either order.
	for _, rows := range [][]*ent.LoyaltyAccount{{busy, linked}, {linked, busy}} {
		if got := pickAccount(tid, rows, nil); got.ID != busy.ID {
			t.Errorf("pickAccount = %s, want the busiest account %s", got.ID, busy.ID)
		}
	}
	// The given contact still beats points.
	if got := pickAccount(tid, []*ent.LoyaltyAccount{busy, linked}, &contactID); got.ID != linked.ID {
		t.Errorf("pickAccount with contact = %s, want the contact's account %s", got.ID, linked.ID)
	}
	// Equal points: the linked row beats an older unlinked one.
	older := &ent.LoyaltyAccount{ID: uuid.New(), LifetimePoints: 10, CreatedAt: t0.Add(-time.Hour)}
	if got := pickAccount(tid, []*ent.LoyaltyAccount{older, linked}, nil); got.ID != linked.ID {
		t.Errorf("pickAccount on a tie = %s, want the linked account %s", got.ID, linked.ID)
	}
	// A zero uuid is not a link, so the oldest wins.
	zeroLink := &ent.LoyaltyAccount{ID: uuid.New(), LifetimePoints: 10, CreatedAt: t0.Add(time.Hour), CrmContactID: &nilLink}
	if got := pickAccount(tid, []*ent.LoyaltyAccount{zeroLink, older}, nil); got.ID != older.ID {
		t.Errorf("pickAccount with a zero link = %s, want the oldest account %s", got.ID, older.ID)
	}

	oldest := &ent.ClientRecord{ID: uuid.New(), CreatedAt: t0}
	linkedRec := &ent.ClientRecord{ID: uuid.New(), CreatedAt: t0.Add(time.Hour), CrmContactID: &contactID}
	if got := pickClient(tid, []*ent.ClientRecord{oldest, linkedRec}, nil); got.ID != linkedRec.ID {
		t.Errorf("pickClient = %s, want the linked record %s", got.ID, linkedRec.ID)
	}
}

func TestFindAccountAmbiguousPrefersLinkedContact(t *testing.T) {
	r := NewResolver(openTestDB(t), nil, zap.NewNop())
	ctx := context.Background()
	tid := uuid.New()
	contactID := uuid.New()
	t0 := time.Date(2025, 1, 1, 9, 0, 0, 0, time.UTC)
	busy := seedLegacy(t, r, tid, "0712345678", 500, t0, nil)
	linked := seedLegacy(t, r, tid, "254712345678", 10, t0.Add(time.Hour), &contactID)

	got, err := FindAccountForContact(ctx, r.db, tid, "+254712345678", &contactID)
	if err != nil {
		t.Fatalf("FindAccountForContact: %v", err)
	}
	if got == nil || got.ID != linked {
		t.Errorf("FindAccountForContact = %v, want the account linked to the contact %s", got, linked)
	}

	// Without the contact id, points come before the link: the busier unlinked row wins.
	other := uuid.New()
	for name, id := range map[string]*uuid.UUID{"no contact": nil, "unknown contact": &other} {
		got, err := FindAccountForContact(ctx, r.db, tid, "+254712345678", id)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got == nil || got.ID != busy {
			t.Errorf("%s: got %v, want the account with more points %s", name, got, busy)
		}
	}

	// The contact given wins over a row linked to another contact.
	otherLinked := seedLegacy(t, r, tid, "712345678", 900, t0.Add(-time.Hour), &other)
	got, err = FindAccountForContact(ctx, r.db, tid, "+254712345678", &contactID)
	if err != nil {
		t.Fatalf("FindAccountForContact: %v", err)
	}
	if got == nil || got.ID != linked {
		t.Errorf("FindAccountForContact = %v, want the account linked to the contact %s", got, linked)
	}
	got, err = FindAccountForContact(ctx, r.db, tid, "+254712345678", nil)
	if err != nil {
		t.Fatalf("FindAccountForContact: %v", err)
	}
	if got == nil || got.ID != otherLinked {
		t.Errorf("no contact: got %v, want the busiest account %s", got, otherLinked)
	}
}

func TestFindClientAmbiguousPrefersLinkedThenOldest(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	contactID := uuid.New()
	t0 := time.Date(2025, 1, 1, 9, 0, 0, 0, time.UTC)
	db.ClientRecord.Create().SetTenantID(tid).SetPhone("0712345678").SetCreatedAt(t0).SaveX(ctx)
	linked := db.ClientRecord.Create().SetTenantID(tid).SetPhone("254712345678").
		SetCreatedAt(t0.Add(time.Hour)).SetCrmContactID(contactID).SaveX(ctx)

	// A linked record beats an older unlinked one, with or without the contact id.
	got, err := FindClientIn(ctx, db, tid, "+254712345678")
	if err != nil {
		t.Fatalf("FindClientIn: %v", err)
	}
	if got == nil || got.ID != linked.ID {
		t.Errorf("FindClientIn = %v, want the linked record %s", got, linked.ID)
	}

	// Nothing linked: the oldest wins.
	plain := uuid.New()
	first := db.ClientRecord.Create().SetTenantID(plain).SetPhone("0712345678").SetCreatedAt(t0).SaveX(ctx)
	db.ClientRecord.Create().SetTenantID(plain).SetPhone("+254712345678").SetCreatedAt(t0.Add(time.Hour)).SaveX(ctx)
	got, err = FindClientIn(ctx, db, plain, "+254712345678")
	if err != nil {
		t.Fatalf("FindClientIn: %v", err)
	}
	if got == nil || got.ID != first.ID {
		t.Errorf("FindClientIn unlinked = %v, want the oldest record %s", got, first.ID)
	}

	got, err = FindClientForContact(ctx, db, tid, "+254712345678", &contactID)
	if err != nil {
		t.Fatalf("FindClientForContact: %v", err)
	}
	if got == nil || got.ID != linked.ID {
		t.Errorf("FindClientForContact = %v, want the linked record %s", got, linked.ID)
	}
}
