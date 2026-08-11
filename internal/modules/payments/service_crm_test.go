package payments

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/platform/marketflow"
)

// TestResolveOrCreateCrmContactID_FallsBackToMarketflowWhenNoLoyaltyLink is the regression test
// for the root-cause bug (boi-enterprises, 2026-08-11): a credit-sale customer with no cached
// LoyaltyAccount CRM link must still get a real, resolvable CRM contact id — not an empty one that
// forces treasury to key their AR row on phone alone, permanently disconnected from whatever CRM
// contact a later flow (opening balance, an invoice) creates for the same person.
func TestResolveOrCreateCrmContactID_FallsBackToMarketflowWhenNoLoyaltyLink(t *testing.T) {
	svc, _ := newTestPaymentsService(t)
	wantID := uuid.New()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/internal/contacts/upsert" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": wantID.String()})
	}))
	defer srv.Close()
	svc.SetMarketFlowClient(marketflow.NewClient(srv.URL, "test-key", zap.NewNop()))

	tid := uuid.New()
	got := svc.ResolveOrCreateCrmContactID(context.Background(), tid, "+254700000005", "Jane Doe")
	if got != wantID.String() {
		t.Errorf("ResolveOrCreateCrmContactID = %q, want %q (the marketflow-upserted contact)", got, wantID.String())
	}
}

// TestResolveOrCreateCrmContactID_PrefersExistingLoyaltyLink proves an existing LoyaltyAccount
// CRM link is used as-is (no marketflow round-trip) — the fallback only kicks in when nothing is
// cached yet.
func TestResolveOrCreateCrmContactID_PrefersExistingLoyaltyLink(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()
	crmID := uuid.New()
	const phone = "+254700000006"

	if _, err := client.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone(phone).SetCustomerName("Existing Loyalty Customer").
		SetCrmContactID(crmID).
		Save(ctx); err != nil {
		t.Fatalf("seed loyalty account: %v", err)
	}

	// No marketflow client configured at all — if this call fell through to the marketflow
	// fallback it would nil-panic or return "", proving the cached link short-circuits correctly.
	got := svc.ResolveOrCreateCrmContactID(ctx, tid, phone, "Existing Loyalty Customer")
	if got != crmID.String() {
		t.Errorf("ResolveOrCreateCrmContactID = %q, want %q (the cached loyalty CRM link)", got, crmID.String())
	}
}

// TestResolveOrCreateCrmContactID_DegradesGracefullyWhenMarketflowUnavailable proves a credit sale
// never fails just because CRM sync is unreachable/unconfigured — it falls back to the old
// phone-only behaviour rather than erroring.
func TestResolveOrCreateCrmContactID_DegradesGracefullyWhenMarketflowUnavailable(t *testing.T) {
	svc, _ := newTestPaymentsService(t)
	// marketflow client left nil (never configured) — Enabled() would be false even if set with no URL.
	got := svc.ResolveOrCreateCrmContactID(context.Background(), uuid.New(), "+254700000007", "No CRM Customer")
	if got != "" {
		t.Errorf("ResolveOrCreateCrmContactID = %q, want empty string when marketflow is unavailable", got)
	}
}
