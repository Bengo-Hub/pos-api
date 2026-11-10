package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/platform/subscriptions"
)

// TestTerminalTokenNeverPlatformOwner locks in that a PIN session in the platform tenant
// ("codevertex") never carries platform owner powers from the tenant slug. It is
// gating-exempt only through the sub_exempt flag that subscriptions-api returns for it.
func TestTerminalTokenNeverPlatformOwner(t *testing.T) {
	client := openSQLiteTestClient(t, "pinowner")
	ctx := context.Background()
	secret := []byte("secret")

	tn, err := client.Tenant.Create().SetID(uuid.New()).SetName("Codevertex").SetSlug("codevertex").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	member, err := client.StaffMember.Create().SetTenantID(tn.ID).SetUserID(uuid.New()).SetName("Till Admin").SetRole("admin").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}

	mint := func(t *testing.T, h *PINAuthHandler) (terminalEntitlements, *terminalClaims) {
		t.Helper()
		subEnt := h.sessionEntitlements(ctx, tn.ID, member)
		token, _, err := issueTerminalJWT(member, tn.ID, uuid.Nil, secret, client, ctx, subEnt)
		if err != nil {
			t.Fatalf("issue terminal JWT: %v", err)
		}
		tc, err := validateTerminalJWT(token, secret)
		if err != nil {
			t.Fatalf("validate terminal JWT: %v", err)
		}
		if subEnt.IsPlatformOwner || tc.IsPlatformOwner || terminalToAuthClaims(tc).IsPlatformOwner {
			t.Fatal("terminal token must never carry is_platform_owner from the tenant slug")
		}
		if tc.IsDemo {
			t.Fatal("the platform tenant is not the demo tenant")
		}
		return subEnt, tc
	}

	t.Run("subscriptions unreachable", func(t *testing.T) {
		h := NewPINAuthHandler(zap.NewNop(), client, secret, nil, "", "")
		subEnt, _ := mint(t, h)
		if subEnt.TenantSlug != "codevertex" {
			t.Fatalf("TenantSlug = %q, want codevertex", subEnt.TenantSlug)
		}
	})

	t.Run("subscriptions returns exempt", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ACTIVE", "billing_mode": "exempt", "exempt": true, "is_perpetual": true,
				"features": []string{"pos_orders"}, "active_service_tags": []string{"pos"},
			})
		}))
		defer srv.Close()
		subs := subscriptions.NewClient(subscriptions.Config{ServiceURL: srv.URL}, zap.NewNop())
		h := NewPINAuthHandler(zap.NewNop(), client, secret, subs, "", "")
		_, tc := mint(t, h)
		if !tc.SubscriptionExempt {
			t.Fatal("exempt tenant must carry sub_exempt")
		}
		if !terminalToAuthClaims(tc).IsGatingExempt() {
			t.Fatal("exempt tenant must stay gating-exempt on a terminal token")
		}
	})
}
