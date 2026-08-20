package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Bengo-Hub/httpware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
)

// newLoyaltyReferralTestHandler mirrors newHeldItemsTestHandler's pattern (same package) but uses
// openSQLiteTestClient (sqlite_testdb_test.go) — its own doc comment documents why that's preferred
// over enttest.Open's shared in-memory mode for this package's test suite.
func newLoyaltyReferralTestHandler(t *testing.T) (*LoyaltyHandler, *ent.Client) {
	t.Helper()
	client := openSQLiteTestClient(t, "loyalty_referral")
	return NewLoyaltyHandler(zap.NewNop(), client, nil), client
}

func referralRequest(t *testing.T, tid, accountID uuid.UUID, body map[string]any) *http.Request {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(buf))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("accountID", accountID.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = httpware.WithTenantID(ctx, tid.String())
	return req.WithContext(ctx)
}

// TestCreateReferral_FormatTolerantMatching covers the 2026-08-20 fix: the referral phone field
// now accepts PhoneInputField's E.164 output, which will almost never exact-match a
// LoyaltyAccount.CustomerPhone stored in national format — both the self-referral guard and the
// idempotent-duplicate check must compare by national subscriber digits too, matching
// CompleteReferral's own tolerance in platform/subscriptions/sale_finalized.go.
func TestCreateReferral_FormatTolerantMatching(t *testing.T) {
	h, client := newLoyaltyReferralTestHandler(t)
	ctx := context.Background()
	tid := uuid.New()

	acc, err := client.LoyaltyAccount.Create().
		SetTenantID(tid).
		SetCustomerPhone("0712345678"). // national format, as stored by the walk-in/CRM flow
		SetCustomerName("Test Customer").
		Save(ctx)
	if err != nil {
		t.Fatalf("seed loyalty account: %v", err)
	}

	t.Run("self-referral rejected even when formats differ", func(t *testing.T) {
		req := referralRequest(t, tid, acc.ID, map[string]any{"referred_phone": "+254712345678"})
		w := httptest.NewRecorder()
		h.CreateReferral(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for self-referral, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("a fresh referral in E.164 succeeds", func(t *testing.T) {
		req := referralRequest(t, tid, acc.ID, map[string]any{"referred_phone": "+254798765432"})
		w := httptest.NewRecorder()
		h.CreateReferral(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("re-submitting the same referral in a different format returns the existing one, not a duplicate", func(t *testing.T) {
		req := referralRequest(t, tid, acc.ID, map[string]any{"referred_phone": "0798765432"}) // national, same subscriber
		w := httptest.NewRecorder()
		h.CreateReferral(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}

		count, err := client.Referral.Query().Where().Count(ctx)
		if err != nil {
			t.Fatalf("count referrals: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 referral row (idempotent match across formats), got %d", count)
		}
	})
}
