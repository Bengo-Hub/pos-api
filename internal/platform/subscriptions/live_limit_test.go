package subscriptions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
)

// A token minted before an upgrade must not keep refusing: the refusal asks the live plan first, and a
// failed lookup keeps the token's answer.
func TestCheckStructuralLimitConsultsTheLivePlan(t *testing.T) {
	t.Cleanup(func() { liveLimitLookup.Store(nil) })
	live, ok := 0, false
	SetLiveLimitLookup(func(context.Context, string, string) (int, bool) { return live, ok })
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/devices", nil)
		return r.WithContext(authclient.ContextWithClaims(r.Context(), &authclient.Claims{TenantID: "t1", SubscriptionLimits: map[string]int{"max_devices": 1}}))
	}
	if CheckStructuralLimit(httptest.NewRecorder(), req(), "devices", "max_devices", 1) {
		t.Fatal("a failed lookup must keep the token's refusal")
	}
	live, ok = 5, true
	if !CheckStructuralLimit(httptest.NewRecorder(), req(), "devices", "max_devices", 1) {
		t.Fatal("an upgrade to 5 devices must allow the second on a stale token")
	}
	if CheckStructuralLimit(httptest.NewRecorder(), req(), "devices", "max_devices", 5) {
		t.Fatal("the live cap still applies")
	}
}
