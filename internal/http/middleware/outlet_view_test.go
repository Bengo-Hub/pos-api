package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// The ?outlet_id= param on the All-Sales list, summary and export follows the header rule: HQ
// callers see any outlet (and "all"), branch staff only their own, assigned or the HQ outlet.
func TestCanViewOutlet(t *testing.T) {
	client := newOutletContextTestClient(t)
	defer client.Close()
	ctx := context.Background()

	tenantID := uuid.New()
	slug := fmt.Sprintf("acme-%s", uuid.NewString())
	if _, err := client.Tenant.Create().SetID(tenantID).SetName("Acme").SetSlug(slug).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	mk := func(code string, hq bool) uuid.UUID {
		o, err := client.Outlet.Create().SetTenantID(tenantID).SetTenantSlug(slug).
			SetCode(code).SetName(code).SetIsHq(hq).Save(ctx)
		if err != nil {
			t.Fatalf("seed outlet %s: %v", code, err)
		}
		return o.ID
	}
	hq, junior, eldoret := mk("HQ", true), mk("JUN", false), mk("ELD", false)

	cashier := uuid.New()
	staff, err := client.StaffMember.Create().SetTenantID(tenantID).SetUserID(cashier).SetName("Cashier").Save(ctx)
	if err != nil {
		t.Fatalf("seed staff: %v", err)
	}
	if _, err := client.StaffOutlet.Create().SetTenantID(tenantID).SetStaffMemberID(staff.ID).SetOutletID(junior).Save(ctx); err != nil {
		t.Fatalf("seed staff outlet: %v", err)
	}

	req := func(claims *authclient.Claims) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/orders", nil)
		return r.WithContext(authclient.ContextWithClaims(r.Context(), claims))
	}
	admin := req(&authclient.Claims{TenantID: tenantID.String(), RegisteredClaims: jwt.RegisteredClaims{Subject: uuid.NewString()}, Roles: []string{"admin"}})
	branch := req(&authclient.Claims{TenantID: tenantID.String(), RegisteredClaims: jwt.RegisteredClaims{Subject: cashier.String()}, Roles: []string{"cashier"}})

	if !CanAccessAllOutlets(admin) || CanAccessAllOutlets(branch) {
		t.Fatalf("all-outlets: admin=%v branch=%v, want true/false", CanAccessAllOutlets(admin), CanAccessAllOutlets(branch))
	}
	cases := []struct {
		name   string
		r      *http.Request
		outlet uuid.UUID
		want   bool
	}{
		{"admin any outlet", admin, eldoret, true},
		{"cashier assigned outlet", branch, junior, true},
		{"cashier HQ outlet", branch, hq, true},
		{"cashier other branch", branch, eldoret, false},
	}
	for _, c := range cases {
		if got := CanViewOutlet(c.r, client, tenantID, c.outlet); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
