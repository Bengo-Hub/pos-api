package handlers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
)

// newResolverTestClient opens a fresh in-memory sqlite ent client (same pure-Go shim used by
// held_items_test.go — the sqlite3 driver is registered once via that file's init()).
func newResolverTestClient(t *testing.T) *ent.Client {
	t.Helper()
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:resolvertest_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// seedRole creates a POSRoleV2 (global if tenantID is uuid.Nil, else tenant-scoped) with the
// given permission codes, creating each POSPermission row fresh (codes are unique per call site
// in these tests, so no dedupe is needed).
func seedRole(t *testing.T, client *ent.Client, tenantID uuid.UUID, roleCode string, isSystem bool, permCodes ...string) *ent.POSRoleV2 {
	t.Helper()
	ctx := context.Background()

	create := client.POSRoleV2.Create().
		SetID(uuid.New()).
		SetRoleCode(roleCode).
		SetName(roleCode).
		SetIsSystemRole(isSystem)
	if tenantID != uuid.Nil {
		create = create.SetTenantID(tenantID)
	}
	role, err := create.Save(ctx)
	if err != nil {
		t.Fatalf("seed role %q: %v", roleCode, err)
	}

	for _, code := range permCodes {
		p, err := client.POSPermission.Create().
			SetID(uuid.New()).
			SetPermissionCode(code).
			SetName(code).
			SetModule("test").
			SetAction("test").
			Save(ctx)
		if err != nil {
			t.Fatalf("seed permission %q: %v", code, err)
		}
		if _, err := role.Update().AddPermissions(p).Save(ctx); err != nil {
			t.Fatalf("attach permission %q to role %q: %v", code, roleCode, err)
		}
	}

	reloaded, err := client.POSRoleV2.Get(ctx, role.ID)
	if err != nil {
		t.Fatalf("reload seeded role: %v", err)
	}
	return reloaded
}

func TestResolveRolePermissions_ExistingRole_ReturnsGrants(t *testing.T) {
	client := newResolverTestClient(t)
	role := seedRole(t, client, uuid.Nil, "waiter", true, "pos.orders.add", "pos.tables.view")

	codes, err := resolveRolePermissions(context.Background(), client, uuid.New(), role.RoleCode)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(codes) != 2 {
		t.Fatalf("expected 2 permission codes, got %d: %v", len(codes), codes)
	}
}

// A role code that legitimately does not exist must return an EMPTY slice with a NIL error —
// this is valid data ("this role has no grants" / "unknown role"), never a failure. Conflating
// the two was never the bug here, but it's the exact distinction the fix depends on, so lock it in.
func TestResolveRolePermissions_UnknownRole_ReturnsEmptyNotError(t *testing.T) {
	client := newResolverTestClient(t)

	codes, err := resolveRolePermissions(context.Background(), client, uuid.New(), "does_not_exist")
	if err != nil {
		t.Fatalf("unknown role must not be an error, got: %v", err)
	}
	if len(codes) != 0 {
		t.Fatalf("expected empty slice for unknown role, got %v", codes)
	}
}

// THE regression test for the 2026-07-19 outage: a genuine query failure (here, a closed DB
// connection) must be returned as an error — NEVER silently swallowed into an empty-but-nil-error
// result. Before the fix, this test would have passed incorrectly (err == nil, codes == []),
// which is exactly the bug: pos-api would 200 a caller with a degraded permission set
// indistinguishable from "role legitimately has zero grants".
func TestResolveRolePermissions_QueryFailure_ReturnsError(t *testing.T) {
	client := newResolverTestClient(t)
	_ = client.Close() // force every subsequent query to fail

	codes, err := resolveRolePermissions(context.Background(), client, uuid.New(), "waiter")
	if err == nil {
		t.Fatal("expected an error from a closed DB connection, got nil — a genuine failure must never be reported as an empty grant set")
	}
	if codes != nil {
		t.Fatalf("expected nil codes on error, got %v", codes)
	}
}

func TestResolveEffectivePermissions_UnionsBaseRoleAndAssignedRole(t *testing.T) {
	client := newResolverTestClient(t)
	ctx := context.Background()
	assignedBy := uuid.New()

	// POSUserRoleAssignment.user_id is a required FK to User, which is itself a required FK to
	// Tenant — seed the whole chain.
	tenant, err := client.Tenant.Create().
		SetID(uuid.New()).
		SetName("Test Tenant").
		SetSlug("test-tenant-" + uuid.NewString()[:8]).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	tenantID := tenant.ID

	posUser, err := client.User.Create().
		SetID(uuid.New()).
		SetTenantID(tenantID).
		SetEmail("waiter@test.local").
		SetFullName("Test Waiter").
		Save(ctx)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	userID := posUser.ID

	base := seedRole(t, client, uuid.Nil, "waiter", true, "pos.orders.add", "pos.tables.view")
	extra := seedRole(t, client, uuid.Nil, "floor_supervisor", true, "pos.orders.view", "pos.reports.view")

	if _, err := client.POSUserRoleAssignment.Create().
		SetTenantID(tenantID).
		SetUserID(userID).
		SetRoleID(extra.ID).
		SetAssignedBy(assignedBy).
		SetAssignedAt(time.Now()).
		Save(context.Background()); err != nil {
		t.Fatalf("seed assignment: %v", err)
	}

	perms, err := resolveEffectivePermissions(context.Background(), client, tenantID, userID, base.RoleCode, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"pos.orders.add": true, "pos.tables.view": true, "pos.orders.view": true, "pos.reports.view": true}
	if len(perms) != len(want) {
		t.Fatalf("expected %d unioned permissions, got %d: %v", len(want), len(perms), perms)
	}
	for _, p := range perms {
		if !want[p] {
			t.Errorf("unexpected permission in union: %q", p)
		}
	}
}

// THE end-to-end regression test: when the underlying resolution chain hits a genuine failure,
// resolveEffectivePermissions must propagate an error, NOT return an empty (or partial) slice
// with a nil error. This is what pos-ui's refreshServicePermissions (polled every 60s) implicitly
// relies on being safe: a non-error response is trustworthy as a COMPLETE permission set.
func TestResolveEffectivePermissions_QueryFailure_ReturnsErrorNotEmptySet(t *testing.T) {
	client := newResolverTestClient(t)
	_ = client.Close()

	perms, err := resolveEffectivePermissions(context.Background(), client, uuid.New(), uuid.New(), "waiter", nil)
	if err == nil {
		t.Fatal("expected an error when the DB is unavailable, got nil — this is the exact class of bug that caused the 2026-07-19 outage (a masked failure presented as a valid empty/partial permission set)")
	}
	if perms != nil {
		t.Fatalf("expected nil permissions on error, got %v", perms)
	}
}

func TestResolveEffectivePermissions_RawGlobalRoleNameMatchesPOSRoleCode(t *testing.T) {
	client := newResolverTestClient(t)
	tenantID := uuid.New()

	// A tenant custom role whose code happens to equal a raw SSO global role name — leg-3
	// resolution (custom roles assigned on the auth side).
	custom := seedRole(t, client, tenantID, "shift_lead", false, "pos.cash_drawers.manage")

	perms, err := resolveEffectivePermissions(context.Background(), client, tenantID, uuid.New(), "cashier", []string{"shift_lead"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, p := range perms {
		if p == "pos.cash_drawers.manage" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the raw-global-role-name leg to pick up %q's grants, got %v", custom.RoleCode, perms)
	}
}
