package customers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// A desk-sent contact belongs to the guest only when the CRM holds the guest's phone or email
// on it. Phone spellings and email case do not matter; empty values never match.
func TestContactMatchesGuest(t *testing.T) {
	c := crm.Contact{ID: uuid.New(), Phone: "+254712345678", Email: "Jane@Example.com"}
	cases := []struct {
		name, phone, email string
		want               bool
	}{
		{"same phone, local spelling", "0712345678", "", true},
		{"same email, other case", "", " jane@example.com ", true},
		{"phone differs, email matches", "0799000111", "jane@example.com", true},
		{"neither matches", "0799000111", "sam@example.com", false},
		{"nothing given", "", "", false},
	}
	for _, tc := range cases {
		if got := ContactMatchesGuest(c, tc.phone, tc.email); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	if ContactMatchesGuest(crm.Contact{ID: uuid.New()}, "", "") {
		t.Error("a contact with no phone or email matches nobody")
	}
}

// ContactMatches asks the CRM for the contact and compares it with the guest. A contact the CRM
// does not know is no match; an unreachable CRM is an error, so the caller does not trust the id.
func TestResolverContactMatches(t *testing.T) {
	known := crm.Contact{ID: uuid.New(), Phone: "+254712345678", Email: "jane@example.com"}
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
			if id == known.ID {
				out = append(out, known)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	}))
	t.Cleanup(srv.Close)
	r := NewResolver(openTestDB(t), crm.New(srv.URL, "test-key", zap.NewNop()), zap.NewNop())
	ctx := context.Background()
	tid := uuid.New()

	if ok, err := r.ContactMatches(ctx, tid, known.ID, "0712345678", ""); err != nil || !ok {
		t.Errorf("matching guest: ok=%v err=%v, want true", ok, err)
	}
	if ok, err := r.ContactMatches(ctx, tid, known.ID, "0799000111", "sam@example.com"); err != nil || ok {
		t.Errorf("other guest: ok=%v err=%v, want false", ok, err)
	}
	if ok, err := r.ContactMatches(ctx, tid, uuid.New(), "0712345678", ""); err != nil || ok {
		t.Errorf("unknown contact: ok=%v err=%v, want false", ok, err)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	rd := NewResolver(openTestDB(t), crm.New(down.URL, "test-key", zap.NewNop()), zap.NewNop())
	if _, err := rd.ContactMatches(ctx, tid, known.ID, "0712345678", ""); err == nil {
		t.Error("an unreachable CRM must be an error, not a match or a mismatch")
	}

	var nilRes *Resolver
	if _, err := nilRes.ContactMatches(ctx, tid, known.ID, "0712345678", ""); err == nil {
		t.Error("no CRM configured must be an error: the id cannot be checked")
	}
}
