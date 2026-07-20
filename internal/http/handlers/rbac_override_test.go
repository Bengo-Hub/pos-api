package handlers

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/modules/rbac"
)

// TestEnsureTenantRoleOverride_CopyOnWrite verifies that editing a shared/global role via the
// copy-on-write path (a) materializes a per-tenant override cloning the global role's current
// permissions, (b) leaves the global role untouched, (c) reuses the same override on a second call,
// and (d) that resolveRolePermissions then prefers the tenant override — so a tenant's uncheck of
// pos.payments.add for "waiter" takes effect for that tenant only. This is the regression guard for
// the recurring "Settle Bill kept showing / the uncheck didn't stick" bug.
func TestEnsureTenantRoleOverride_CopyOnWrite(t *testing.T) {
	client := newResolverTestClient(t)
	ctx := context.Background()
	repo := rbac.NewEntRepository(client)

	tenant := uuid.New()
	// Global waiter with a payments grant (mirrors the real seed default).
	global := seedRole(t, client, uuid.Nil, "waiter", true, "pos.payments.add", "pos.orders.add", "pos.tables.view")

	// First edit → materialize the override, cloning the global perms.
	overrideID, err := repo.EnsureTenantRoleOverride(ctx, tenant, global.ID)
	if err != nil {
		t.Fatalf("EnsureTenantRoleOverride: %v", err)
	}
	if overrideID == global.ID {
		t.Fatalf("expected a NEW override id, got the global role id %s", overrideID)
	}
	clone, err := repo.GetRolePermissions(ctx, overrideID)
	if err != nil {
		t.Fatalf("get override perms: %v", err)
	}
	if len(clone) != 3 {
		t.Fatalf("override should clone all 3 global perms, got %d", len(clone))
	}

	// Second call is idempotent — same override, no duplicate row.
	again, err := repo.EnsureTenantRoleOverride(ctx, tenant, global.ID)
	if err != nil {
		t.Fatalf("EnsureTenantRoleOverride (2nd): %v", err)
	}
	if again != overrideID {
		t.Fatalf("expected the same override id on reuse, got %s vs %s", again, overrideID)
	}

	// Drop pos.payments.add from the OVERRIDE only.
	var payID uuid.UUID
	for _, p := range clone {
		if p.PermissionCode == "pos.payments.add" {
			payID = p.ID
		}
	}
	if payID == uuid.Nil {
		t.Fatal("could not find pos.payments.add in the clone")
	}
	if err := repo.RemovePermissionFromRole(ctx, overrideID, payID); err != nil {
		t.Fatalf("remove perm from override: %v", err)
	}

	// The tenant now resolves WITHOUT payments (override preferred)...
	tenantCodes, err := resolveRolePermissions(ctx, client, tenant, "waiter")
	if err != nil {
		t.Fatalf("resolveRolePermissions (tenant): %v", err)
	}
	if contains(tenantCodes, "pos.payments.add") {
		t.Fatalf("tenant override should NOT contain pos.payments.add, got %v", tenantCodes)
	}

	// ...while the GLOBAL role (any other tenant) still has it — no cross-tenant leakage.
	otherCodes, err := resolveRolePermissions(ctx, client, uuid.New(), "waiter")
	if err != nil {
		t.Fatalf("resolveRolePermissions (other tenant): %v", err)
	}
	if !contains(otherCodes, "pos.payments.add") {
		t.Fatalf("other tenants must still inherit the global pos.payments.add, got %v", otherCodes)
	}
}

// TestEnsureTenantRoleOverride_AlreadyTenantOwned edits a tenant-owned custom role in place (no copy).
func TestEnsureTenantRoleOverride_AlreadyTenantOwned(t *testing.T) {
	client := newResolverTestClient(t)
	ctx := context.Background()
	repo := rbac.NewEntRepository(client)

	tenant := uuid.New()
	custom := seedRole(t, client, tenant, "night_cashier", false, "pos.orders.add")

	got, err := repo.EnsureTenantRoleOverride(ctx, tenant, custom.ID)
	if err != nil {
		t.Fatalf("EnsureTenantRoleOverride: %v", err)
	}
	if got != custom.ID {
		t.Fatalf("tenant-owned role should be edited in place, got %s want %s", got, custom.ID)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
