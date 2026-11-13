package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Bengo-Hub/httpware"
	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/customers"
)

func tenantPost(t *testing.T, tid uuid.UUID, body map[string]any) *http.Request {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(buf))
	return req.WithContext(httpware.WithTenantID(req.Context(), tid.String()))
}

// TestBulkClientImportSavesUnlinked checks the bulk import never calls the CRM: a new row is
// saved unlinked in E.164 for the reconciler, and a re-imported deleted-contact row loses its
// marker while keeping its other metadata.
func TestBulkClientImportSavesUnlinked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("bulk import called the CRM: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	db := openSQLiteTestClient(t, "client_bulk_unlinked")
	ctx := context.Background()
	tid := uuid.New()
	marked := db.ClientRecord.Create().
		SetTenantID(tid).SetPhone("+254722000111").
		SetMetadata(map[string]any{customers.UnlinkedAtKey: "2026-11-01T10:00:00Z", "source": "import"}).
		SaveX(ctx)

	ch := NewClientHandler(zap.NewNop(), db)
	ch.SetCustomerResolver(customers.NewResolver(db, crm.New(srv.URL, "test-key", zap.NewNop()), zap.NewNop()))
	w := httptest.NewRecorder()
	ch.BulkImport(w, tenantPost(t, tid, map[string]any{"clients": []map[string]any{
		{"external_ref": "a", "phone": "0712345678"},
		{"external_ref": "b", "phone": "0722000111", "notes": "returning"},
	}}))
	if w.Code != http.StatusOK {
		t.Fatalf("BulkImport status = %d, body %s", w.Code, w.Body.String())
	}

	created := db.ClientRecord.Query().AllX(ctx)
	var fresh int
	for _, rec := range created {
		if rec.ID == marked.ID {
			continue
		}
		fresh++
		if rec.Phone != "+254712345678" || rec.CrmContactID != nil {
			t.Errorf("new row = phone %q link %v, want +254712345678 unlinked", rec.Phone, rec.CrmContactID)
		}
	}
	if fresh != 1 {
		t.Errorf("%d new rows, want 1 (the second row must update the existing record)", fresh)
	}
	got := db.ClientRecord.GetX(ctx, marked.ID)
	if customers.IsUnlinked(got.Metadata) || got.Metadata["source"] != "import" || got.Notes != "returning" {
		t.Errorf("re-imported row = metadata %v notes %q, want marker removed, other keys and notes kept", got.Metadata, got.Notes)
	}
}

// TestS2SEarnAmbiguousDoesNotCreateDuplicate stores one number in two legacy spellings and
// resolves an S2S earn for its E.164 form. The account with more points is reused, no third
// account is created and the CRM is not called. S2SEarn credits the account this returns; the
// handler itself is not driven here because applyEarn locks with FOR UPDATE, which SQLite lacks.
func TestS2SEarnAmbiguousDoesNotCreateDuplicate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("earn on an existing number called the CRM: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	db := openSQLiteTestClient(t, "s2s_ambiguous")
	ctx := context.Background()
	tid := uuid.New()
	db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("0712345678").SetCustomerName("Jane Doe").
		SetPointsBalance(40).SetLifetimePoints(40).
		SaveX(ctx)
	busy := db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("254712345678").SetCustomerName("Jane Doe").
		SetPointsBalance(120).SetLifetimePoints(300).
		SaveX(ctx)

	lh := NewLoyaltyHandler(zap.NewNop(), db, customers.NewResolver(db, crm.New(srv.URL, "test-key", zap.NewNop()), zap.NewNop()))
	for i := 0; i < 2; i++ {
		acc, err := lh.findOrCreateAccountByPhone(ctx, tid, "+254712345678", "Jane Doe")
		if err != nil {
			t.Fatalf("findOrCreateAccountByPhone: %v", err)
		}
		if acc == nil || acc.ID != busy.ID {
			t.Fatalf("resolved account %v, want the legacy account with more points %s", acc, busy.ID)
		}
		// The same credit S2SEarn applies to the resolved id.
		db.LoyaltyAccount.UpdateOneID(acc.ID).AddPointsBalance(10).AddLifetimePoints(10).ExecX(ctx)
	}
	if n := db.LoyaltyAccount.Query().CountX(ctx); n != 2 {
		t.Errorf("%d loyalty accounts after earning, want the 2 legacy accounts and no new one", n)
	}
	if got := db.LoyaltyAccount.GetX(ctx, busy.ID); got.PointsBalance != 140 {
		t.Errorf("chosen account balance = %d, want 140", got.PointsBalance)
	}
}

// TestStaffCreateRelinksUnlinked covers a customer whose CRM contact was deleted (marker set,
// link cleared) and who is registered again by staff: the account and the client record are
// linked again, the marker is removed, other metadata keys and points are kept.
func TestStaffCreateRelinksUnlinked(t *testing.T) {
	newContact := uuid.New()
	var upserts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		upserts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(crm.Contact{
			ID: newContact, FirstName: body["first_name"], LastName: body["last_name"],
			Email: body["email"], Phone: body["phone"],
		})
	}))
	t.Cleanup(srv.Close)

	db := openSQLiteTestClient(t, "customer_relink")
	res := customers.NewResolver(db, crm.New(srv.URL, "test-key", zap.NewNop()), zap.NewNop())
	ctx := context.Background()
	tid := uuid.New()
	marked := map[string]any{customers.UnlinkedAtKey: "2026-11-01T10:00:00Z", "source": "import"}

	acc := db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("+254712345678").SetCustomerName("Jane Doe").
		SetPointsBalance(75).SetMetadata(marked).
		SaveX(ctx)
	rec := db.ClientRecord.Create().
		SetTenantID(tid).SetPhone("+254712345678").SetMetadata(marked).
		SaveX(ctx)

	lh := NewLoyaltyHandler(zap.NewNop(), db, res)
	w := httptest.NewRecorder()
	lh.CreateAccount(w, tenantPost(t, tid, map[string]any{
		"customer_phone": "0712345678", "customer_name": "Jane Doe", "customer_email": "jane@example.com",
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("CreateAccount status = %d, body %s", w.Code, w.Body.String())
	}
	gotAcc := db.LoyaltyAccount.GetX(ctx, acc.ID)
	if gotAcc.CrmContactID == nil || *gotAcc.CrmContactID != newContact {
		t.Errorf("account crm_contact_id = %v, want %s", gotAcc.CrmContactID, newContact)
	}
	if customers.IsUnlinked(gotAcc.Metadata) {
		t.Error("account still carries the unlinked marker")
	}
	if gotAcc.Metadata["source"] != "import" {
		t.Errorf("account metadata = %v, want other keys kept", gotAcc.Metadata)
	}
	if gotAcc.PointsBalance != 75 {
		t.Errorf("points = %d, want 75 kept", gotAcc.PointsBalance)
	}
	if n := db.LoyaltyAccount.Query().CountX(ctx); n != 1 {
		t.Errorf("%d loyalty accounts, want the existing one reused", n)
	}

	ch := NewClientHandler(zap.NewNop(), db)
	ch.SetCustomerResolver(res)
	w = httptest.NewRecorder()
	ch.CreateOrUpsert(w, tenantPost(t, tid, map[string]any{"phone": "0712345678", "notes": "back again"}))
	if w.Code != http.StatusOK {
		t.Fatalf("CreateOrUpsert status = %d, body %s", w.Code, w.Body.String())
	}
	gotRec := db.ClientRecord.GetX(ctx, rec.ID)
	if gotRec.CrmContactID == nil || *gotRec.CrmContactID != newContact {
		t.Errorf("client crm_contact_id = %v, want %s", gotRec.CrmContactID, newContact)
	}
	if customers.IsUnlinked(gotRec.Metadata) || gotRec.Metadata["source"] != "import" {
		t.Errorf("client metadata = %v, want marker removed and other keys kept", gotRec.Metadata)
	}
	if upserts.Load() != 2 {
		t.Errorf("made %d CRM upserts, want 2", upserts.Load())
	}
}

// TestBulkClientImportDropsUntrustedContactIDs: a row's crm_contact_id is stored only when the
// CRM returns it for this tenant with the row's phone; a foreign id is dropped and the row is
// saved unlinked for the reconciler (S2-8 review M-8). The CRM is asked once for the whole file.
func TestBulkClientImportDropsUntrustedContactIDs(t *testing.T) {
	mine, foreign := uuid.New(), uuid.New()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/internal/contacts/batch" {
			t.Errorf("unexpected CRM call %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": mine.String(), "phone": "+254712345678"},
		}})
	}))
	t.Cleanup(srv.Close)

	db := openSQLiteTestClient(t, "client_bulk_trust")
	ctx := context.Background()
	tid := uuid.New()
	ch := NewClientHandler(zap.NewNop(), db)
	ch.SetCustomerResolver(customers.NewResolver(db, crm.New(srv.URL, "test-key", zap.NewNop()), zap.NewNop()))
	w := httptest.NewRecorder()
	ch.BulkImport(w, tenantPost(t, tid, map[string]any{"clients": []map[string]any{
		{"external_ref": "a", "phone": "0712345678", "crm_contact_id": mine.String()},
		{"external_ref": "b", "phone": "0722000111", "crm_contact_id": foreign.String()},
	}}))
	if w.Code != http.StatusOK {
		t.Fatalf("BulkImport status = %d, body %s", w.Code, w.Body.String())
	}
	if calls.Load() != 1 {
		t.Errorf("%d CRM calls, want 1", calls.Load())
	}
	for _, rec := range db.ClientRecord.Query().AllX(ctx) {
		switch rec.Phone {
		case "+254712345678":
			if rec.CrmContactID == nil || *rec.CrmContactID != mine {
				t.Errorf("trusted row link = %v, want %s", rec.CrmContactID, mine)
			}
		case "+254722000111":
			if rec.CrmContactID != nil {
				t.Errorf("foreign id was stored: %v", rec.CrmContactID)
			}
		}
	}
}
