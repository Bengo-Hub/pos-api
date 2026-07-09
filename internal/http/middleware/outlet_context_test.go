package middleware

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/Bengo-Hub/httpware"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
)

// ── pure-Go sqlite shim (mirrors internal/http/handlers/held_items_test.go) ────────────────
// ent expects a driver registered as "sqlite3"; modernc.org/sqlite (no cgo — this repo builds
// with CGO_ENABLED=0) registers as "sqlite". Bridge it and force foreign keys on per connection.

type sqlite3Driver struct{ *sqlite.Driver }

func (d sqlite3Driver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	if execer, ok := conn.(interface {
		Exec(string, []driver.Value) (driver.Result, error)
	}); ok {
		if _, err := execer.Exec("PRAGMA foreign_keys = ON;", nil); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func init() {
	sql.Register("sqlite3", sqlite3Driver{Driver: &sqlite.Driver{}})
}

func newOutletContextTestClient(t *testing.T) *ent.Client {
	t.Helper()
	return enttest.Open(t, "sqlite3", fmt.Sprintf("file:outletctx_%s?mode=memory&cache=shared", uuid.NewString()))
}

// TestOutletContextMiddleware_ClaimFallback covers the fallback tier added for terminal (PIN)
// sessions: when the request carries no X-Outlet-ID header, the middleware should resolve the
// outlet from the JWT claim's outlet_id BEFORE falling back to the tenant's HQ outlet.
func TestOutletContextMiddleware_ClaimFallback(t *testing.T) {
	client := newOutletContextTestClient(t)
	defer client.Close()
	ctx := context.Background()

	tenantID := uuid.New()
	_, err := client.Tenant.Create().
		SetID(tenantID).
		SetName("Acme").
		SetSlug(fmt.Sprintf("acme-%s", uuid.NewString())).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	hqOutlet, err := client.Outlet.Create().
		SetTenantID(tenantID).
		SetTenantSlug("acme").
		SetCode("HQ").
		SetName("Headquarters").
		SetIsHq(true).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed HQ outlet: %v", err)
	}

	branchUseCase := "hospitality"
	branchOutlet, err := client.Outlet.Create().
		SetTenantID(tenantID).
		SetTenantSlug("acme").
		SetCode("BR1").
		SetName("Branch One").
		SetIsHq(false).
		SetUseCase(branchUseCase).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed branch outlet: %v", err)
	}

	mw := OutletContextMiddleware(client, zap.NewNop())

	run := func(claims *authclient.Claims) *OutletContext {
		var captured *OutletContext
		handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			captured = OutletFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/tables", nil)
		if claims != nil {
			req = req.WithContext(authclient.ContextWithClaims(req.Context(), claims))
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return captured
	}

	t.Run("no header, valid outlet claim resolves that outlet (not HQ)", func(t *testing.T) {
		claims := &authclient.Claims{
			TenantID: tenantID.String(),
			OutletID: branchOutlet.ID.String(),
		}
		got := run(claims)
		if got == nil {
			t.Fatal("expected resolved outlet, got nil")
		}
		if got.ID != branchOutlet.ID {
			t.Fatalf("expected branch outlet %s, got %s (HQ=%s)", branchOutlet.ID, got.ID, hqOutlet.ID)
		}
		if got.UseCase != branchUseCase {
			t.Fatalf("expected use_case %q, got %q", branchUseCase, got.UseCase)
		}
	})

	t.Run("no header, no outlet claim falls back to HQ as before", func(t *testing.T) {
		claims := &authclient.Claims{TenantID: tenantID.String()}
		got := run(claims)
		if got == nil {
			t.Fatal("expected resolved outlet, got nil")
		}
		if got.ID != hqOutlet.ID {
			t.Fatalf("expected HQ fallback %s, got %s", hqOutlet.ID, got.ID)
		}
	})

	t.Run("no header, garbage outlet claim falls back to HQ", func(t *testing.T) {
		claims := &authclient.Claims{TenantID: tenantID.String(), OutletID: "not-a-uuid"}
		got := run(claims)
		if got == nil {
			t.Fatal("expected resolved outlet, got nil")
		}
		if got.ID != hqOutlet.ID {
			t.Fatalf("expected HQ fallback %s, got %s", hqOutlet.ID, got.ID)
		}
	})

	t.Run("explicit header still wins over the claim", func(t *testing.T) {
		// HQ-capable claim so the header path's non-HQ-user assignment check is bypassed —
		// this subtest is only exercising "header wins over claim", not the 403 assignment path.
		claims := &authclient.Claims{TenantID: tenantID.String(), OutletID: hqOutlet.ID.String(), IsHQUser: true}
		handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := OutletFromContext(r.Context())
			if got == nil || got.ID != branchOutlet.ID {
				t.Fatalf("expected header outlet %s, got %+v", branchOutlet.ID, got)
			}
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/tables", nil)
		req.Header.Set(httpware.HeaderOutletID, branchOutlet.ID.String())
		req = req.WithContext(authclient.ContextWithClaims(req.Context(), claims))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	})
}
