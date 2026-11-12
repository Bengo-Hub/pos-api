package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posrolev2"
	"github.com/bengobox/pos-service/internal/ent/posuserroleassignment"
	"github.com/bengobox/pos-service/internal/modules/rbac"
)

// rbacFixture seeds the shared system roles the tier ceilings come from: cashier (staff tier)
// with pos.orders.view, manager with pos.orders.view and pos.staff.manage, admin with those plus
// pos.config.manage.
type rbacFixture struct {
	client *ent.Client
	h      *RBACHandler
	perms  map[string]*ent.POSPermission
}

func newRBACFixture(t *testing.T, name string) *rbacFixture {
	t.Helper()
	client := openSQLiteTestClient(t, name)
	ctx := context.Background()
	repo := rbac.NewEntRepository(client)
	f := &rbacFixture{
		client: client,
		h:      NewRBACHandler(zap.NewNop(), rbac.NewService(repo, zap.NewNop()), repo, "", ""),
		perms:  map[string]*ent.POSPermission{},
	}
	for _, code := range []string{"pos.orders.view", "pos.staff.manage", "pos.config.manage"} {
		f.perms[code] = client.POSPermission.Create().SetPermissionCode(code).SetName(code).
			SetModule("test").SetAction("manage").SaveX(ctx)
	}
	f.globalRole("cashier", "pos.orders.view")
	f.globalRole("waiter", "pos.orders.view")
	f.globalRole("manager", "pos.orders.view", "pos.staff.manage")
	f.globalRole("admin", "pos.orders.view", "pos.staff.manage", "pos.config.manage")
	return f
}

func (f *rbacFixture) link(roleID uuid.UUID, codes ...string) {
	for _, c := range codes {
		f.client.POSRolePermission.Create().SetRoleID(roleID).SetPermissionID(f.perms[c].ID).ExecX(context.Background())
	}
}

func (f *rbacFixture) globalRole(code string, perms ...string) *ent.POSRoleV2 {
	r := f.client.POSRoleV2.Create().SetRoleCode(code).SetName(code).SetIsSystemRole(true).SaveX(context.Background())
	f.link(r.ID, perms...)
	return r
}

func (f *rbacFixture) tenantRole(tid uuid.UUID, code string, perms ...string) *ent.POSRoleV2 {
	r := f.client.POSRoleV2.Create().SetTenantID(tid).SetRoleCode(code).SetName(code).SaveX(context.Background())
	f.link(r.ID, perms...)
	return r
}

func (f *rbacFixture) user(tid uuid.UUID) uuid.UUID {
	id := uuid.New()
	f.client.User.Create().SetID(id).SetAuthServiceUserID(id).SetTenantID(tid).
		SetEmail(id.String() + "@example.com").SetFullName("Person").ExecX(context.Background())
	return id
}

func (f *rbacFixture) assign(t *testing.T, tid uuid.UUID, caller *authclient.Claims, userID, roleID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.h.AssignRole(rec, secRequest(t, http.MethodPost, tid,
		map[string]any{"user_id": userID.String(), "role_id": roleID.String()}, caller, nil))
	return rec
}

func (f *rbacFixture) assignments(tid, userID uuid.UUID) int {
	return f.client.POSUserRoleAssignment.Query().
		Where(posuserroleassignment.TenantID(tid), posuserroleassignment.UserID(userID)).
		CountX(context.Background())
}

// withPerms is caller claims carrying canonical permission codes, as a PIN or SSO token does.
func withPerms(c *authclient.Claims, perms ...string) *authclient.Claims {
	c.Permissions = perms
	return c
}

// TestCrossTenantRoleAssignmentRefused locks in S2-14 item 1. An admin of tenant B makes a role
// in B; as a manager (or even an admin) in tenant A they cannot assign that role id in A, and an
// assignment row that names it anyway grants nothing in A.
func TestCrossTenantRoleAssignmentRefused(t *testing.T) {
	f := newRBACFixture(t, "seccrossrole")
	ctx := context.Background()
	tidA, tidB := uuid.New(), uuid.New()
	seedTenant(t, f.client, tidA)
	seedTenant(t, f.client, tidB)
	roleB := f.tenantRole(tidB, "b_boss", "pos.orders.view", "pos.staff.manage", "pos.config.manage")

	attacker := f.user(tidA)
	manager := withPerms(secClaims(tidA, attacker, "https://sso.example", false, "manager"), "pos.orders.view", "pos.staff.manage")
	admin := secClaims(tidA, uuid.New(), "https://sso.example", false, "admin")
	for name, caller := range map[string]*authclient.Claims{"manager": manager, "admin": admin} {
		if rec := f.assign(t, tidA, caller, attacker, roleB.ID); rec.Code != http.StatusNotFound {
			t.Fatalf("%s assigning tenant B's role in A: status = %d, body = %s; want 404", name, rec.Code, rec.Body.String())
		}
	}
	if f.assignments(tidA, attacker) != 0 {
		t.Fatal("no assignment row may be written for another tenant's role")
	}

	// The service and repository refuse it too, whoever calls them.
	svc := rbac.NewService(rbac.NewEntRepository(f.client), zap.NewNop())
	if err := svc.AssignRole(ctx, tidA, attacker, roleB.ID, uuid.New()); !errors.Is(err, rbac.ErrRoleNotInTenant) {
		t.Fatalf("service AssignRole with another tenant's role: err = %v; want ErrRoleNotInTenant", err)
	}

	// A row planted directly (as before this fix) grants nothing in A.
	f.client.POSUserRoleAssignment.Create().SetTenantID(tidA).SetUserID(attacker).SetRoleID(roleB.ID).
		SetAssignedBy(attacker).SetAssignedAt(time.Now()).ExecX(ctx)
	perms, err := resolveEffectivePermissions(ctx, f.client, tidA, attacker, "cashier", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range perms {
		if p == "pos.config.manage" || p == "pos.staff.manage" {
			t.Fatalf("a planted cross-tenant assignment must grant nothing, got %v", perms)
		}
	}
	if codes := resolveAssignedRoleCodes(ctx, f.client, tidA, attacker); len(codes) != 0 {
		t.Fatalf("a cross-tenant role must not show as assigned, got %v", codes)
	}
	if priv, err := userHasPrivilegedPOSAssignment(ctx, f.client, tidA, attacker); err != nil || priv {
		t.Fatalf("a cross-tenant role must not count for the tier, got %v %v", priv, err)
	}
	if has, err := svc.HasAnyPermission(ctx, tidA, attacker, "pos.config.manage"); err != nil || has {
		t.Fatalf("the middleware's per-user leg must ignore the cross-tenant role, got %v %v", has, err)
	}

	// Tenant B's own role still works in B for an admin there.
	adminB := secClaims(tidB, uuid.New(), "https://sso.example", false, "admin")
	member := f.user(tidB)
	if rec := f.assign(t, tidB, adminB, member, roleB.ID); rec.Code != http.StatusCreated {
		t.Fatalf("admin of B assigning B's role in B: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestReservedRoleCodesRefused locks in the I-3 code rule: a custom role may not take a system
// or platform role's code, an alias of one, or any admin or manager tier code.
func TestReservedRoleCodesRefused(t *testing.T) {
	f := newRBACFixture(t, "secreserved")
	tid := uuid.New()
	seedTenant(t, f.client, tid)
	admin := secClaims(tid, uuid.New(), "https://sso.example", false, "admin")

	create := func(code string) int {
		rec := httptest.NewRecorder()
		f.h.CreateRole(rec, secRequest(t, http.MethodPost, tid, map[string]any{"role_code": code, "name": "X"}, admin, nil))
		return rec.Code
	}
	for _, code := range []string{
		"admin", " Owner ", "superuser", "superusers", "Super-Admin", "platform_admin", "platform_owner",
		"manager", "Store Manager", "supervisor", "cashier", "staff", "waiter", "WAITER",
		// auth-api's self-service roles (I-A): anyone may take these at sign-up.
		"customer", "Member", "student", "parent", "driver", "rider",
	} {
		if got := create(code); got != http.StatusBadRequest {
			t.Fatalf("creating custom role %q: status = %d; want 400", code, got)
		}
	}
	if got := create("floor_lead"); got != http.StatusCreated {
		t.Fatalf("creating an ordinary custom role: status = %d; want 201", got)
	}
}

// TestRolePermissionCeiling locks in the I-3 ceiling: a caller below admin cannot leave a role
// carrying a permission they do not hold; an admin can.
func TestRolePermissionCeiling(t *testing.T) {
	f := newRBACFixture(t, "secceiling")
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, f.client, tid)
	custom := f.tenantRole(tid, "floor_lead", "pos.orders.view")
	manager := withPerms(secClaims(tid, uuid.New(), "https://sso.example", false, "manager"), "pos.orders.view", "pos.staff.manage")
	admin := secClaims(tid, uuid.New(), "https://sso.example", false, "admin")

	set := func(caller *authclient.Claims, roleID uuid.UUID, codes ...string) *httptest.ResponseRecorder {
		ids := make([]string, 0, len(codes))
		for _, c := range codes {
			ids = append(ids, f.perms[c].ID.String())
		}
		rec := httptest.NewRecorder()
		f.h.SetRolePermissions(rec, secRequest(t, http.MethodPut, tid, map[string]any{"permission_ids": ids}, caller,
			map[string]string{"roleID": roleID.String()}))
		return rec
	}
	held := func() map[string]bool {
		out := map[string]bool{}
		for _, p := range f.client.POSRoleV2.QueryPermissions(custom).AllX(ctx) {
			out[p.PermissionCode] = true
		}
		return out
	}

	if rec := set(manager, custom.ID, "pos.orders.view", "pos.config.manage"); rec.Code != http.StatusForbidden {
		t.Fatalf("manager granting a permission they lack: status = %d, body = %s; want 403", rec.Code, rec.Body.String())
	}
	if held()["pos.config.manage"] {
		t.Fatal("a refused edit must not grant anything")
	}
	// Holding a permission is not enough when it would lift the role above staff (I-B).
	if rec := set(manager, custom.ID, "pos.orders.view", "pos.staff.manage"); rec.Code != http.StatusForbidden {
		t.Fatalf("manager lifting a role to manager tier: status = %d, body = %s; want 403", rec.Code, rec.Body.String())
	}
	if rec := set(manager, custom.ID, "pos.orders.view"); rec.Code != http.StatusOK {
		t.Fatalf("manager keeping a role at staff tier with permissions they hold: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := set(admin, custom.ID, "pos.orders.view", "pos.config.manage"); rec.Code != http.StatusOK {
		t.Fatalf("admin granting any permission: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Another tenant's role id is not found (fail closed), not edited.
	other := uuid.New()
	seedTenant(t, f.client, other)
	foreign := f.tenantRole(other, "their_role", "pos.orders.view")
	if rec := set(admin, foreign.ID, "pos.config.manage"); rec.Code != http.StatusNotFound {
		t.Fatalf("editing another tenant's role: status = %d; want 404", rec.Code)
	}
}

// TestAssignmentTierCountsCustomRolePermissions locks in the I-3 tier rule: a custom role is
// tiered by its permissions as well as its code, so a manager cannot assign a harmlessly named
// role carrying manager or admin grants, and such an assignment counts as privileged and clears
// the person's PIN.
func TestAssignmentTierCountsCustomRolePermissions(t *testing.T) {
	f := newRBACFixture(t, "seccustomtier")
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, f.client, tid)
	staffish := f.tenantRole(tid, "floor_helper", "pos.orders.view")
	managerish := f.tenantRole(tid, "floor_lead", "pos.orders.view", "pos.staff.manage")
	adminish := f.tenantRole(tid, "night_desk", "pos.config.manage")
	manager := withPerms(secClaims(tid, uuid.New(), "https://sso.example", false, "manager"),
		"pos.orders.view", "pos.staff.manage", "pos.config.manage")

	repo := rbac.NewEntRepository(f.client)
	for role, want := range map[*ent.POSRoleV2]int{staffish: 0, managerish: 1, adminish: 2} {
		if got, err := repo.RoleTier(ctx, tid, role.ID); err != nil || got != want {
			t.Fatalf("RoleTier(%s) = %d, %v; want %d", role.RoleCode, got, err, want)
		}
	}

	person := f.user(tid)
	for _, role := range []*ent.POSRoleV2{managerish, adminish} {
		if rec := f.assign(t, tid, manager, person, role.ID); rec.Code != http.StatusForbidden {
			t.Fatalf("manager assigning %s: status = %d, body = %s; want 403", role.RoleCode, rec.Code, rec.Body.String())
		}
	}
	if rec := f.assign(t, tid, manager, person, staffish.ID); rec.Code != http.StatusCreated {
		t.Fatalf("manager assigning a staff-tier custom role: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if priv, err := userHasPrivilegedPOSAssignment(ctx, f.client, tid, person); err != nil || priv {
		t.Fatalf("a staff-tier custom role is not privileged, got %v %v", priv, err)
	}

	// An admin assigns the admin-grade custom role: it counts as privileged, and it clears the
	// PIN of the person's cashier row.
	row := f.client.StaffMember.Create().SetTenantID(tid).SetUserID(person).SetName("Till").SetRole("cashier").
		SetPinHash("hash").SetPinFastHash("fast").SaveX(ctx)
	admin := secClaims(tid, uuid.New(), "https://sso.example", false, "admin")
	if rec := f.assign(t, tid, admin, person, adminish.ID); rec.Code != http.StatusCreated {
		t.Fatalf("admin assigning an admin-grade custom role: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if priv, err := userHasPrivilegedPOSAssignment(ctx, f.client, tid, person); err != nil || !priv {
		t.Fatalf("an admin-grade custom role is privileged, got %v %v", priv, err)
	}
	if got := f.client.StaffMember.GetX(ctx, row.ID); got.PinHash != nil {
		t.Fatal("assigning an admin-grade custom role must clear the PIN")
	}
}

// TestSelfServiceRolesGetNoPOSPermissions locks in I-A: a person whose token holds only auth's
// self-service roles (a storefront customer, a learner, a parent, a rider applicant) maps to no
// POS role, and a tenant role named after a self-service role (made before codes were reserved)
// reaches nobody through the global-role legs. An invited "member" (invite only in a business
// tenant since 2026-11-10) is staff and still gets the cashier role.
func TestSelfServiceRolesGetNoPOSPermissions(t *testing.T) {
	f := newRBACFixture(t, "secselfservice")
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, f.client, tid)
	// A legacy tenant role named "customer" holding a POS permission.
	f.tenantRole(tid, "customer", "pos.orders.view")
	svc := rbac.NewService(rbac.NewEntRepository(f.client), zap.NewNop())

	for _, roles := range [][]string{{"customer"}, {"customer", "student"}, {"student", "parent", "driver"}} {
		if got := rbac.MapGlobalRolesToServiceRole(roles); got != "" {
			t.Fatalf("MapGlobalRolesToServiceRole(%v) = %q; self-service roles map to no POS role", roles, got)
		}
		if has, err := svc.HasAnyPermissionViaGlobalRoles(ctx, tid, roles, "pos.orders.view"); err != nil || has {
			t.Fatalf("route permission leg for %v: has = %v, err = %v; want no permissions", roles, has, err)
		}
		perms, err := resolveEffectivePermissions(ctx, f.client, tid, uuid.New(), rbac.MapGlobalRolesToServiceRole(roles), roles)
		if err != nil || len(perms) != 0 {
			t.Fatalf("/auth/me permissions for %v = %v, %v; want none", roles, perms, err)
		}
	}
	// A real staff role next to a self-service one still resolves.
	if got := rbac.MapGlobalRolesToServiceRole([]string{"customer", "cashier"}); got != "cashier" {
		t.Fatalf("customer plus cashier must still map to cashier, got %q", got)
	}

	// Invited member-only staff keep the cashier role and its permissions.
	for _, roles := range [][]string{{"member"}, {"member", "customer"}} {
		if got := rbac.MapGlobalRolesToServiceRole(roles); got != "cashier" {
			t.Fatalf("MapGlobalRolesToServiceRole(%v) = %q; an invited member is cashier", roles, got)
		}
		if has, err := svc.HasAnyPermissionViaGlobalRoles(ctx, tid, roles, "pos.orders.view"); err != nil || !has {
			t.Fatalf("route permission leg for %v: has = %v, err = %v; want the cashier's grants", roles, has, err)
		}
	}
}

// TestManagerStaysWithinStaffTierRoles locks in I-B and M-3: a manager may not lift a staff-tier
// role they already assigned into manager tier, shrink, rename or delete a role above staff tier,
// or revoke someone's assignment of one. An admin may.
func TestManagerStaysWithinStaffTierRoles(t *testing.T) {
	f := newRBACFixture(t, "secstafftier")
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, f.client, tid)
	manager := withPerms(secClaims(tid, uuid.New(), "https://sso.example", false, "manager"),
		"pos.orders.view", "pos.staff.manage", "pos.config.manage")
	admin := secClaims(tid, uuid.New(), "https://sso.example", false, "admin")
	floor := f.tenantRole(tid, "floor", "pos.orders.view")
	built := f.tenantRole(tid, "night_desk", "pos.orders.view", "pos.staff.manage", "pos.config.manage")

	setPerms := func(caller *authclient.Claims, roleID uuid.UUID, codes ...string) int {
		ids := make([]string, 0, len(codes))
		for _, c := range codes {
			ids = append(ids, f.perms[c].ID.String())
		}
		rec := httptest.NewRecorder()
		f.h.SetRolePermissions(rec, secRequest(t, http.MethodPut, tid, map[string]any{"permission_ids": ids}, caller,
			map[string]string{"roleID": roleID.String()}))
		return rec.Code
	}
	permsOf := func(r *ent.POSRoleV2) int { return f.client.POSRoleV2.QueryPermissions(r).CountX(ctx) }

	// Raise after assign: the manager hands out "floor", then tries to lift it.
	x := f.user(tid)
	if rec := f.assign(t, tid, manager, x, floor.ID); rec.Code != http.StatusCreated {
		t.Fatalf("manager assigning a staff-tier role: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if code := setPerms(manager, floor.ID, "pos.orders.view", "pos.staff.manage"); code != http.StatusForbidden {
		t.Fatalf("manager lifting an assigned role to manager tier: status = %d; want 403", code)
	}
	if permsOf(floor) != 1 {
		t.Fatal("a refused raise must not change the role")
	}

	// Shrink an admin-built role: refused for the manager, allowed for the admin.
	if code := setPerms(manager, built.ID, "pos.orders.view"); code != http.StatusForbidden {
		t.Fatalf("manager shrinking an admin-grade role: status = %d; want 403", code)
	}
	if permsOf(built) != 3 {
		t.Fatal("a refused shrink must not change the role")
	}

	// Rename and delete an admin-grade role: refused for the manager.
	rec := httptest.NewRecorder()
	f.h.UpdateRole(rec, secRequest(t, http.MethodPatch, tid, map[string]any{"name": "Renamed"}, manager,
		map[string]string{"roleID": built.ID.String()}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("manager renaming an admin-grade role: status = %d; want 403", rec.Code)
	}
	del := func(caller *authclient.Claims, roleID uuid.UUID) int {
		rec := httptest.NewRecorder()
		f.h.DeleteRole(rec, secRequest(t, http.MethodDelete, tid, nil, caller, map[string]string{"roleID": roleID.String()}))
		return rec.Code
	}
	if code := del(manager, built.ID); code != http.StatusForbidden {
		t.Fatalf("manager deleting an admin-grade role: status = %d; want 403", code)
	}

	// Revoke: the admin gives Y the admin-grade role; the manager cannot take it away.
	y := f.user(tid)
	if rec := f.assign(t, tid, admin, y, built.ID); rec.Code != http.StatusCreated {
		t.Fatalf("admin assigning the admin-grade role: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	assignmentID := func(userID, roleID uuid.UUID) uuid.UUID {
		return f.client.POSUserRoleAssignment.Query().Where(posuserroleassignment.TenantID(tid),
			posuserroleassignment.UserID(userID), posuserroleassignment.RoleID(roleID)).OnlyX(ctx).ID
	}
	revoke := func(caller *authclient.Claims, id uuid.UUID) int {
		rec := httptest.NewRecorder()
		f.h.RevokeRole(rec, secRequest(t, http.MethodDelete, tid, nil, caller, map[string]string{"id": id.String()}))
		return rec.Code
	}
	if code := revoke(manager, assignmentID(y, built.ID)); code != http.StatusForbidden {
		t.Fatalf("manager revoking an admin-grade assignment: status = %d; want 403", code)
	}
	if f.assignments(tid, y) != 1 {
		t.Fatal("a refused revoke must leave the assignment")
	}
	// Staff-tier assignments stay the manager's to revoke.
	if code := revoke(manager, assignmentID(x, floor.ID)); code != http.StatusOK {
		t.Fatalf("manager revoking a staff-tier assignment: status = %d; want 200", code)
	}

	// The admin can do all of it.
	if code := revoke(admin, assignmentID(y, built.ID)); code != http.StatusOK {
		t.Fatalf("admin revoking: status = %d", code)
	}
	if code := setPerms(admin, built.ID, "pos.orders.view"); code != http.StatusOK {
		t.Fatalf("admin shrinking: status = %d", code)
	}
	if code := del(admin, built.ID); code != http.StatusOK {
		t.Fatalf("admin deleting: status = %d", code)
	}
}

// TestTenantSystemRoleCopyTieredByPermissions locks in M-2: a tenant's copy of a system role is
// tiered by its permissions too, while the shared system role keeps its code tier.
func TestTenantSystemRoleCopyTieredByPermissions(t *testing.T) {
	f := newRBACFixture(t, "secsystemcopy")
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, f.client, tid)
	repo := rbac.NewEntRepository(f.client)
	copyRole := f.client.POSRoleV2.Create().SetTenantID(tid).SetRoleCode("cashier").SetName("Cashier").
		SetIsSystemRole(true).SaveX(ctx)
	f.link(copyRole.ID, "pos.orders.view", "pos.config.manage")
	if got, err := repo.RoleTier(ctx, tid, copyRole.ID); err != nil || got != 2 {
		t.Fatalf("a tenant cashier copy with admin-grade permissions: tier %d, %v; want 2", got, err)
	}
	shared := f.client.POSRoleV2.Query().Where(posrolev2.RoleCode("cashier"), posrolev2.TenantIDIsNil()).OnlyX(ctx)
	if got, err := repo.RoleTier(ctx, tid, shared.ID); err != nil || got != 0 {
		t.Fatalf("the shared cashier role: tier %d, %v; want 0", got, err)
	}
}
