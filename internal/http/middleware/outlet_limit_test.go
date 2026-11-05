package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
)

// A manager given a list of outlets keeps to it, even with a manager-level role that would otherwise
// see every outlet. With no list nothing changes.
func TestOutletLimitOutranksManagerRole(t *testing.T) {
	held, other := uuid.New(), uuid.New()

	request := func(c *authclient.Claims) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		return r.WithContext(authclient.ContextWithClaims(context.Background(), c))
	}

	limited := request(&authclient.Claims{Roles: []string{"manager"}, OutletIDs: []string{held.String()}})
	if CanAccessAllOutlets(limited) {
		t.Fatal("limited manager must not read across all outlets")
	}
	if !CanViewOutlet(limited, nil, uuid.New(), held) {
		t.Fatal("limited manager must view an outlet on their list")
	}
	if CanViewOutlet(limited, nil, uuid.New(), other) {
		t.Fatal("limited manager must not view an outlet off their list")
	}

	if unset := request(&authclient.Claims{Roles: []string{"manager"}}); !CanAccessAllOutlets(unset) {
		t.Fatal("a manager with no list stays tenant-wide")
	}

	admin := request(&authclient.Claims{Roles: []string{"admin"}, OutletIDs: []string{held.String()}})
	if !CanAccessAllOutlets(admin) {
		t.Fatal("an admin is always tenant-wide, whatever the list says")
	}
}
