package payments

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// refundCapturingStub records every /refunds body treasury receives.
func refundCapturingStub(t *testing.T) (*treasury.Client, func() []treasury.RefundRequest) {
	t.Helper()
	var mu sync.Mutex
	var bodies []treasury.RefundRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/refunds") {
			var body treasury.RefundRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			bodies = append(bodies, body)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(treasury.RefundResponse{ID: uuid.NewString()})
	}))
	t.Cleanup(srv.Close)
	return treasury.NewClient(srv.URL, "test-key", 2*time.Second), func() []treasury.RefundRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]treasury.RefundRequest(nil), bodies...)
	}
}

// Voiding a payment books the reversal against the payment's own method, so an MTN, Airtel,
// Other or M-Pesa code payment no longer comes off the cash account.
func TestVoidPaymentRefundsToThePaymentsOwnMethod(t *testing.T) {
	cases := map[string]string{
		"mtn_momo_manual":     "mtn_momo_manual",
		"airtel_money_manual": "airtel_money_manual",
		"other_manual":        "other_manual",
		"mpesa_manual":        "mpesa_manual",
		"card_manual":         "card_manual",
		"cash":                "cash",
	}
	for method, want := range cases {
		svc, client := newTestPaymentsService(t)
		tc, sent := refundCapturingStub(t)
		svc.SetTreasuryClient(tc)
		tid := uuid.New()
		order := seedPaidOrder(t, client, tid, paid{method, 100, StatusCompleted})
		p := client.POSPayment.Query().OnlyX(context.Background())

		if _, err := svc.VoidPayment(context.Background(), tid, "acme", order.ID, p.ID, uuid.New(), "keyed twice"); err != nil {
			t.Fatalf("%s: VoidPayment: %v", method, err)
		}
		got := sent()
		if len(got) != 1 {
			t.Fatalf("%s: %d refund calls, want 1", method, len(got))
		}
		if got[0].RefundChannel != want {
			t.Errorf("%s: refund_channel = %q, want %q", method, got[0].RefundChannel, want)
		}
	}
}
