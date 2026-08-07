package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	entstaffoutlet "github.com/bengobox/pos-service/internal/ent/staffoutlet"
)

// newStaffTestHandler mirrors newHeldItemsTestHandler's sqlite-memory harness ("sqlite3" is
// already registered by held_items_test.go's init() in this same package — do not re-register).
func newStaffTestHandler(t *testing.T) (*StaffHandler, *ent.Client) {
	t.Helper()
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:stafftest_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	return NewStaffHandler(zap.NewNop(), client, "", ""), client
}

// seedTenant creates the Tenant row Outlet's required edge needs — Outlet.tenant_id has a
// Required() ent edge, so seeding an outlet against an unknown tenant id fails the sqlite FK
// constraint (foreign_keys=ON, see held_items_test.go's driver shim).
func seedTenant(t *testing.T, client *ent.Client, tid uuid.UUID) *ent.Tenant {
	t.Helper()
	tn, err := client.Tenant.Create().
		SetID(tid).
		SetName("Test Tenant").
		SetSlug("test-tenant-" + tid.String()[:8]).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return tn
}

func seedOutlet(t *testing.T, client *ent.Client, tid uuid.UUID, code string) *ent.Outlet {
	t.Helper()
	o, err := client.Outlet.Create().
		SetTenantID(tid).
		SetTenantSlug("test-tenant").
		SetCode(code).
		SetName("Outlet " + code).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed outlet: %v", err)
	}
	return o
}

func seedStaffMember(t *testing.T, client *ent.Client, tid uuid.UUID, role string, homeOutletID uuid.UUID) *ent.StaffMember {
	t.Helper()
	m, err := client.StaffMember.Create().
		SetTenantID(tid).
		SetUserID(uuid.New()).
		SetName("Test Staffer").
		SetRole(role).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed staff member: %v", err)
	}
	if _, err := client.StaffOutlet.Create().
		SetTenantID(tid).
		SetStaffMemberID(m.ID).
		SetOutletID(homeOutletID).
		SetIsHomeOutlet(true).
		Save(context.Background()); err != nil {
		t.Fatalf("seed staff outlet: %v", err)
	}
	return m
}

// staffPatchRequest builds a PATCH /pos/staff/{staffID} request with chi URL params and auth
// claims wired the way the router middleware would (mirrors heldItemRequest's pattern).
func staffPatchRequest(t *testing.T, tid, staffID uuid.UUID, body map[string]any, roles []string) *http.Request {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPatch, "/", bytes.NewReader(buf))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("staffID", staffID.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = httpware.WithTenantID(ctx, tid.String())
	ctx = authclient.ContextWithClaims(ctx, &authclient.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: uuid.NewString()},
		TenantID:         tid.String(),
		Roles:            roles,
	})
	return req.WithContext(ctx)
}

// TestUpdateStaff_SwitchOutlet_MovesHomeOutletExactlyOnce guards the "two home outlets" bug: a
// naive upsert-only outlet reassignment left the OLD outlet's StaffOutlet row is_home_outlet=true
// as well as the new one, so ListStaffForAdmin's .Where(IsHomeOutlet(true)).Limit(1) query could
// non-deterministically keep showing the staff member's PREVIOUS outlet after an admin "switched"
// them — the switch never actually took effect from the admin's point of view.
func TestUpdateStaff_SwitchOutlet_MovesHomeOutletExactlyOnce(t *testing.T) {
	h, client := newStaffTestHandler(t)
	tid := uuid.New()
	seedTenant(t, client, tid)
	outletA := seedOutlet(t, client, tid, "A")
	outletB := seedOutlet(t, client, tid, "B")
	member := seedStaffMember(t, client, tid, "cashier", outletA.ID)

	req := staffPatchRequest(t, tid, member.ID, map[string]any{"outlet_id": outletB.ID.String()}, []string{"admin"})
	rec := httptest.NewRecorder()
	h.UpdateStaff(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("UpdateStaff: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	homeRows, err := client.StaffOutlet.Query().
		Where(entstaffoutlet.StaffMemberID(member.ID), entstaffoutlet.IsHomeOutlet(true)).
		All(context.Background())
	if err != nil {
		t.Fatalf("query staff outlets: %v", err)
	}
	if len(homeRows) != 1 {
		t.Fatalf("expected exactly ONE home outlet after switching, got %d", len(homeRows))
	}
	if homeRows[0].OutletID != outletB.ID {
		t.Fatalf("home outlet = %s, want the NEW outlet %s", homeRows[0].OutletID, outletB.ID)
	}

	// Outlet A's row must still exist (assignment history preserved) but no longer be home.
	oldRow, err := client.StaffOutlet.Query().
		Where(entstaffoutlet.StaffMemberID(member.ID), entstaffoutlet.OutletID(outletA.ID)).
		Only(context.Background())
	if err != nil {
		t.Fatalf("query old outlet row: %v", err)
	}
	if oldRow.IsHomeOutlet {
		t.Fatalf("old outlet A row should have been demoted, still is_home_outlet=true")
	}
}

// TestUpdateStaff_SwitchOutlet_BackToSameOutletIsIdempotent covers the no-op case: re-submitting
// the staff member's CURRENT outlet must not error and must leave exactly one home outlet.
func TestUpdateStaff_SwitchOutlet_BackToSameOutletIsIdempotent(t *testing.T) {
	h, client := newStaffTestHandler(t)
	tid := uuid.New()
	seedTenant(t, client, tid)
	outletA := seedOutlet(t, client, tid, "A")
	member := seedStaffMember(t, client, tid, "cashier", outletA.ID)

	req := staffPatchRequest(t, tid, member.ID, map[string]any{"outlet_id": outletA.ID.String()}, []string{"admin"})
	rec := httptest.NewRecorder()
	h.UpdateStaff(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("UpdateStaff: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	homeRows, err := client.StaffOutlet.Query().
		Where(entstaffoutlet.StaffMemberID(member.ID), entstaffoutlet.IsHomeOutlet(true)).
		All(context.Background())
	if err != nil {
		t.Fatalf("query staff outlets: %v", err)
	}
	if len(homeRows) != 1 {
		t.Fatalf("expected exactly ONE home outlet, got %d", len(homeRows))
	}
}
