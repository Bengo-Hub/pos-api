package payments

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/customers"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// TestResolveReconcileTarget_PrefersLiveBalanceOverStaleEventPayload is the regression test for
// the live boi-enterprises bug (order 002527, MR PETER FUNYULA, 2026-09-10): a mixed Edit-Sale
// increase+reduction fired two balance_updated events in quick succession — the reduction's event
// carried a transient, soon-superseded outstanding_debit (22,700) that landed before the
// increase's own treasury GL call posted the correct final figure (35,410). Trusting the event
// payload directly permanently fabricated a phantom "paid" amount for the gap. The fix re-fetches
// treasury's CURRENT live balance instead — this proves that live figure wins over whatever the
// event itself claims, even when the event's own figure is stale-but-not-obviously-wrong.
func TestResolveReconcileTarget_PrefersLiveBalanceOverStaleEventPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(treasury.CreditTermsResponse{
			OutstandingDebit: "35410", // the TRUE current balance, after the increase leg landed
		})
	}))
	defer srv.Close()

	tc := treasury.NewClient(srv.URL, "test-key", 2*time.Second)
	target, ok := resolveReconcileTarget(context.Background(), tc, uuid.New(), "48589629-b930-4ebb-b72a-5f099acbf7ae", "", "22700")
	if !ok {
		t.Fatal("expected ok=true on a successful live fetch")
	}
	if target != 35410 {
		t.Errorf("target = %v, want 35410 (the LIVE treasury balance) — the stale event payload figure (22700) must never win", target)
	}
}

// TestCustomerDeletedKeepsAccountWithPoints checks that treasury.customer.deleted never deletes a
// loyalty account that still holds points: it is unlinked and marked instead. A zero-point
// account for another deleted customer is removed.
func TestCustomerDeletedKeepsAccountWithPoints(t *testing.T) {
	_, client := newTestPaymentsService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	sub := NewTreasurySubscriber(client, nil, zap.NewNop())

	pointedCRM, zeroCRM := uuid.New(), uuid.New()
	pointed, err := client.LoyaltyAccount.Create().
		SetTenantID(tenantID).SetCustomerPhone("+254711000001").SetCustomerName("Pointed").
		SetCrmContactID(pointedCRM).SetPointsBalance(120).
		SetMetadata(map[string]any{"keep": "yes"}).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed pointed account: %v", err)
	}
	zero, err := client.LoyaltyAccount.Create().
		SetTenantID(tenantID).SetCustomerPhone("+254711000002").SetCustomerName("Zero").
		SetCrmContactID(zeroCRM).SetPointsBalance(0).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed zero account: %v", err)
	}

	sub.cleanupDeletedCustomer(ctx, tenantID, &pointedCRM, "")
	sub.cleanupDeletedCustomer(ctx, tenantID, &zeroCRM, "")

	got, err := client.LoyaltyAccount.Get(ctx, pointed.ID)
	if err != nil {
		t.Fatalf("account with points must survive: %v", err)
	}
	if got.CrmContactID != nil {
		t.Errorf("crm_contact_id = %v, want cleared", got.CrmContactID)
	}
	if got.PointsBalance != 120 {
		t.Errorf("points = %d, want 120", got.PointsBalance)
	}
	if _, ok := got.Metadata[customers.UnlinkedAtKey].(string); !ok {
		t.Errorf("metadata %v is missing %s", got.Metadata, customers.UnlinkedAtKey)
	}
	if got.Metadata["keep"] != "yes" {
		t.Errorf("existing metadata key lost: %v", got.Metadata)
	}
	if _, err := client.LoyaltyAccount.Get(ctx, zero.ID); !ent.IsNotFound(err) {
		t.Errorf("zero-point account should be deleted, got err=%v", err)
	}
}

// TestResolveReconcileTarget_NilClientFallsBackToEventPayload confirms the pre-existing fail-open
// posture is preserved when no treasury client is wired at all.
func TestResolveReconcileTarget_NilClientFallsBackToEventPayload(t *testing.T) {
	target, ok := resolveReconcileTarget(context.Background(), nil, uuid.New(), "some-crm-id", "", "22700")
	if !ok {
		t.Fatal("expected ok=true with a nil client (fallback path)")
	}
	if target != 22700 {
		t.Errorf("target = %v, want 22700 (the event payload figure, since no client is wired)", target)
	}
}

// TestResolveReconcileTarget_LiveFetchErrorSkipsRatherThanFallsBack confirms a failed live fetch
// does NOT silently fall back to the (possibly stale) event figure — the caller must skip that
// reconcile pass instead, since acting on an unverified figure is exactly the bug being fixed.
func TestResolveReconcileTarget_LiveFetchErrorSkipsRatherThanFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	tc := treasury.NewClient(srv.URL, "test-key", 2*time.Second)
	_, ok := resolveReconcileTarget(context.Background(), tc, uuid.New(), "some-crm-id", "", "22700")
	if ok {
		t.Fatal("expected ok=false on a live-fetch error — must not fall back to the stale event figure")
	}
}
