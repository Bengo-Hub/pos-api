package payments

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// TestRunStoreCreditOffset_SendsPhoneFallbackAlongsideResolvedCrmID is the regression test for the
// store-credit-into-a-fresh-credit-sale offset (recordCreditSale's ApplyStoreCredit branch) having
// the exact same customer-key-resolution gap that credit_settlement.go's creditSettlementKey was
// fixed for on 2026-09-14 (boi-enterprises, KELVIN PORT): a resolved crm_contact_id can legitimately
// be one the customer's EXISTING treasury balance row never had (their first credit sale posted
// phone-only, before a CRM contact got linked for that phone). Without also sending the phone as a
// fallback identifier, treasury's apply-to-debt lookup fails outright with "no accounts-receivable
// balance found" and the store credit silently never nets against the new debt — the cashier sees
// the "Apply store credit" checkbox pre-checked and believes it worked, but nothing was applied.
// This confirms the outgoing apply-to-debt request carries BOTH: the resolved crm_contact_id in the
// URL path AND the order's phone in the body's customer_identifier field.
func TestRunStoreCreditOffset_SendsPhoneFallbackAlongsideResolvedCrmID(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	orderID := uuid.New()
	seedOrderForPayment(t, client, orderID)

	crmContactID := uuid.New()
	const phone = "0722000111"

	var body treasury.ApplyToDebtRequest
	var urlKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		// Path shape: /api/v1/s2s/{tenant}/ar/customers/{key}/apply-to-debt
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) >= 2 {
			urlKey = parts[len(parts)-2]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(treasury.ApplyToDebtResponse{ID: crmContactID.String()})
	}))
	defer server.Close()
	svc.SetTreasuryClient(treasury.NewClient(server.URL, "test-key", 2*time.Second))

	svc.runStoreCreditOffset(context.Background(), orderID, "demo-tenant", crmContactID.String(), phone, 500, "ORD-TEST", uuid.New())

	if urlKey != crmContactID.String() {
		t.Errorf("URL key = %q, want the resolved crm_contact_id %s", urlKey, crmContactID)
	}
	if body.CustomerIdentifier != phone {
		t.Errorf("body.customer_identifier = %q, want the order's phone %q — without this fallback, "+
			"a phone-only balance row (the KELVIN PORT shape) can never be found", body.CustomerIdentifier, phone)
	}
}

// TestRunStoreCreditOffset_NoFallbackWhenKeyIsAlreadyThePhone guards the other half of
// creditSettlementKey's rule (mirrored here): when the customer never resolved a CRM contact and
// the offset is keyed on the raw phone directly, there is nothing to fall back FROM — sending the
// same phone again as customer_identifier would be redundant, not wrong, but the call site
// deliberately leaves it empty in that case, so this locks that in.
func TestRunStoreCreditOffset_NoFallbackWhenKeyIsAlreadyThePhone(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	orderID := uuid.New()
	seedOrderForPayment(t, client, orderID)
	const phone = "0722000111"

	var body treasury.ApplyToDebtRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(treasury.ApplyToDebtResponse{ID: phone})
	}))
	defer server.Close()
	svc.SetTreasuryClient(treasury.NewClient(server.URL, "test-key", 2*time.Second))

	svc.runStoreCreditOffset(context.Background(), orderID, "demo-tenant", phone, "", 500, "ORD-TEST", uuid.New())

	if body.CustomerIdentifier != "" {
		t.Errorf("body.customer_identifier = %q, want empty when the URL key is already the phone", body.CustomerIdentifier)
	}
}
