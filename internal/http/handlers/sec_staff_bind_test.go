package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/bengobox/pos-service/internal/ent"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
	"github.com/bengobox/pos-service/internal/modules/rbac"
)

// bindAuthMember is one tenant member as auth-api's S2S member list returns it.
type bindAuthMember struct {
	ID    uuid.UUID
	Email string
	Roles []string
}

// bindFakeAuth stands in for the auth-api S2S endpoints CreateStaff uses: the tenant member list
// (with roles), the membership lookup, member provisioning and the platform-owner lookup.
type bindFakeAuth struct {
	srv               *httptest.Server
	members           []bindAuthMember
	platformOwners    map[uuid.UUID]bool
	provisioned       uuid.UUID
	provisionRoles    []string
	provisions        atomic.Int32
	failList          bool
	failPlatformOwner bool
}

func newBindFakeAuth(t *testing.T) *bindFakeAuth {
	t.Helper()
	f := &bindFakeAuth{platformOwners: map[uuid.UUID]bool{}, provisioned: uuid.New(), provisionRoles: []string{"cashier"}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users"):
			// /api/v1/s2s/{tenant}/users
			if f.failList {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			out := make([]map[string]any, 0, len(f.members))
			for _, m := range f.members {
				out = append(out, map[string]any{"id": m.ID.String(), "email": m.Email, "roles": m.Roles})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/platform-owner"):
			if f.failPlatformOwner {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			uid, _ := uuid.Parse(parts[len(parts)-2])
			_ = json.NewEncoder(w).Encode(map[string]any{"is_platform_owner": f.platformOwners[uid]})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/outlet-limit"):
			uid, _ := uuid.Parse(parts[len(parts)-2])
			for _, m := range f.members {
				if m.ID == uid {
					_ = json.NewEncoder(w).Encode(map[string]any{"outlet_ids": []string{}})
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/members"):
			f.provisions.Add(1)
			var body struct {
				Email string `json:"email"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			// An existing member named by email comes back as themselves (find-or-create).
			for _, m := range f.members {
				if strings.EqualFold(m.Email, body.Email) {
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(map[string]any{"user_id": m.ID.String(), "roles": m.Roles})
					return
				}
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"user_id": f.provisioned.String(), "roles": f.provisionRoles})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	t.Setenv("AUTH_API_URL", f.srv.URL)
	return f
}

// seedAdminAssignment gives userID a POS role assignment (role code roleCode) on tenant tid, the
// per-user grant a staff row bound to that user inherits.
func seedAdminAssignment(t *testing.T, client *ent.Client, tid, userID uuid.UUID, roleCode string) {
	t.Helper()
	ctx := context.Background()
	if err := client.User.Create().SetID(userID).SetAuthServiceUserID(userID).SetTenantID(tid).
		SetEmail(userID.String() + "@example.com").SetFullName("Assigned").Exec(ctx); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	role, err := client.POSRoleV2.Create().SetTenantID(tid).SetRoleCode(roleCode).SetName(roleCode).Save(ctx)
	if err != nil {
		t.Fatalf("seed role: %v", err)
	}
	if _, err := client.POSUserRoleAssignment.Create().SetTenantID(tid).SetUserID(userID).SetRoleID(role.ID).
		SetAssignedBy(uuid.New()).SetAssignedAt(time.Now()).Save(ctx); err != nil {
		t.Fatalf("seed assignment: %v", err)
	}
}

// TestManagerCannotBindStaffToAdminTierMember locks in S2-12 (a): a manager may not create a
// staff row bound to a person who is admin or manager tier in auth, in pos's role assignments or
// as a platform owner, whichever way the person is named (user_id or email). Such a row would
// inherit the person's role assignments, and a later auth sync would raise its role while it
// kept the PIN the manager chose.
func TestManagerCannotBindStaffToAdminTierMember(t *testing.T) {
	auth := newBindFakeAuth(t)
	client := openSQLiteTestClient(t, "secbind")
	h := NewStaffHandler(zap.NewNop(), client, auth.srv.URL, "internal-key")
	tid := uuid.New()
	seedTenant(t, client, tid)
	outlet := seedOutlet(t, client, tid, "A")
	manager := secClaims(tid, uuid.New(), "https://sso.example", false, "manager")
	admin := secClaims(tid, uuid.New(), "https://sso.example", false, "admin")

	tenantAdmin := bindAuthMember{ID: uuid.New(), Email: "boss@example.com", Roles: []string{"admin"}}
	tenantOwner := bindAuthMember{ID: uuid.New(), Email: "owner@example.com", Roles: []string{"staff", "superuser"}}
	assigned := bindAuthMember{ID: uuid.New(), Email: "assigned@example.com", Roles: []string{"staff"}}
	platformOwner := bindAuthMember{ID: uuid.New(), Email: "platform@example.com", Roles: []string{"staff"}}
	cashier := bindAuthMember{ID: uuid.New(), Email: "till@example.com", Roles: []string{"cashier"}}
	auth.members = []bindAuthMember{tenantAdmin, tenantOwner, assigned, platformOwner, cashier}
	auth.platformOwners[platformOwner.ID] = true
	seedAdminAssignment(t, client, tid, assigned.ID, "owner")

	create := func(claims *authclient.Claims, body map[string]any) *httptest.ResponseRecorder {
		body["outlet_id"] = outlet.ID.String()
		body["name"] = "Till Staff"
		body["role"] = "cashier"
		body["pin"] = "4829"
		rec := httptest.NewRecorder()
		h.CreateStaff(rec, secRequest(t, http.MethodPost, tid, body, claims, nil))
		return rec
	}
	rowsFor := func(uid uuid.UUID) int {
		n, err := client.StaffMember.Query().Where(entstaff.TenantID(tid), entstaff.UserID(uid)).Count(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	for name, m := range map[string]bindAuthMember{
		"auth admin": tenantAdmin, "auth superuser": tenantOwner,
		"pos owner assignment": assigned, "platform owner": platformOwner,
	} {
		if rec := create(manager, map[string]any{"user_id": m.ID.String()}); rec.Code != http.StatusForbidden {
			t.Fatalf("manager binding %s by user_id: status = %d, body = %s; want 403", name, rec.Code, rec.Body.String())
		}
		if rowsFor(m.ID) != 0 {
			t.Fatalf("manager binding %s by user_id must not write a staff row", name)
		}
	}

	// By email: refused before auth-api is asked to provision anything.
	before := auth.provisions.Load()
	for _, m := range []bindAuthMember{tenantAdmin, tenantOwner} {
		if rec := create(manager, map[string]any{"email": strings.ToUpper(m.Email)}); rec.Code != http.StatusForbidden {
			t.Fatalf("manager binding %s by email: status = %d, body = %s; want 403", m.Email, rec.Code, rec.Body.String())
		}
	}
	if auth.provisions.Load() != before {
		t.Fatal("a refused bind must not provision or change the person in auth-api")
	}

	// Provisioned by email, but auth answers with an admin-tier membership: refused, no row.
	auth.provisionRoles = []string{"cashier", "admin"}
	if rec := create(manager, map[string]any{"email": "new-admin@example.com"}); rec.Code != http.StatusForbidden {
		t.Fatalf("manager whose provisioned person is admin tier: status = %d; want 403", rec.Code)
	}
	if rowsFor(auth.provisioned) != 0 {
		t.Fatal("a provisioned admin-tier person must not get a manager-made staff row")
	}
	auth.provisionRoles = []string{"cashier"}

	// By email, for people whose tier shows only after provisioning (a POS owner assignment, a
	// platform owner): refused, and no staff row.
	for name, m := range map[string]bindAuthMember{"pos owner assignment": assigned, "platform owner": platformOwner} {
		if rec := create(manager, map[string]any{"email": m.Email}); rec.Code != http.StatusForbidden {
			t.Fatalf("manager binding %s by email: status = %d, body = %s; want 403", name, rec.Code, rec.Body.String())
		}
		if rowsFor(m.ID) != 0 {
			t.Fatalf("manager binding %s by email must not write a staff row", name)
		}
	}

	// The member list cannot be read: a manager is refused (fail closed).
	auth.failList = true
	if rec := create(manager, map[string]any{"user_id": cashier.ID.String()}); rec.Code != http.StatusBadGateway {
		t.Fatalf("manager bind with auth unreadable: status = %d; want 502", rec.Code)
	}
	auth.failList = false

	// The platform-owner lookup fails: refused too (fail closed), no row.
	auth.failPlatformOwner = true
	if rec := create(manager, map[string]any{"user_id": cashier.ID.String()}); rec.Code != http.StatusBadGateway {
		t.Fatalf("manager bind with the platform-owner lookup failing: status = %d; want 502", rec.Code)
	}
	if rowsFor(cashier.ID) != 0 {
		t.Fatal("a bind refused for a failed lookup must not write a staff row")
	}
	auth.failPlatformOwner = false

	// An ordinary cashier member is still fine for a manager.
	if rec := create(manager, map[string]any{"user_id": cashier.ID.String()}); rec.Code != http.StatusCreated {
		t.Fatalf("manager binding a cashier: status = %d, body = %s; want 201", rec.Code, rec.Body.String())
	}
	if rowsFor(cashier.ID) != 1 {
		t.Fatal("a manager may still add an ordinary cashier")
	}

	// An admin-level caller may bind an admin-tier member.
	for _, m := range []bindAuthMember{tenantAdmin, assigned} {
		if rec := create(admin, map[string]any{"user_id": m.ID.String()}); rec.Code != http.StatusCreated {
			t.Fatalf("admin binding %s: status = %d, body = %s; want 201", m.Email, rec.Code, rec.Body.String())
		}
		if rowsFor(m.ID) != 1 {
			t.Fatalf("admin binding %s must write the staff row", m.Email)
		}
	}
}

// TestRoleRaiseClearsPIN locks in S2-12 (b) for staff edits: raising a row into a higher tier
// clears its PIN (the person sets a new one); a move within a tier keeps it.
func TestRoleRaiseClearsPIN(t *testing.T) {
	t.Setenv("AUTH_API_URL", "")
	h, client := newStaffTestHandler(t)
	tid := uuid.New()
	seedTenant(t, client, tid)
	outlet := seedOutlet(t, client, tid, "A")
	admin := secClaims(tid, uuid.New(), "https://sso.example", false, "admin")
	ctx := context.Background()

	withPIN := func(role string) *ent.StaffMember {
		m := seedStaffMember(t, client, tid, role, outlet.ID)
		hash, _ := bcrypt.GenerateFromPassword([]byte("4829"), bcrypt.MinCost)
		return client.StaffMember.UpdateOne(m).SetPinHash(string(hash)).SetPinFastHash("fast-" + uuid.NewString()).SaveX(ctx)
	}
	patch := func(m *ent.StaffMember, role string) *ent.StaffMember {
		rec := httptest.NewRecorder()
		h.UpdateStaff(rec, secRequest(t, http.MethodPatch, tid, map[string]any{"role": role}, admin,
			map[string]string{"staffID": m.ID.String()}))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("UpdateStaff to %q: status = %d, body = %s", role, rec.Code, rec.Body.String())
		}
		return client.StaffMember.GetX(ctx, m.ID)
	}

	for _, step := range []struct{ from, to string }{{"cashier", "manager"}, {"manager", "owner"}, {"waiter", "admin"}} {
		got := patch(withPIN(step.from), step.to)
		if got.PinHash != nil || got.PinFastHash != nil {
			t.Fatalf("%s -> %s must clear the PIN", step.from, step.to)
		}
	}
	for _, step := range []struct{ from, to string }{{"cashier", "waiter"}, {"admin", "owner"}, {"manager", "cashier"}} {
		got := patch(withPIN(step.from), step.to)
		if got.PinHash == nil {
			t.Fatalf("%s -> %s is not a raise and must keep the PIN", step.from, step.to)
		}
	}
}

// TestManagerCannotResetPINOfAdminAssignedRow locks in S2-12 (c): a staff row whose person holds
// an admin or manager POS role assignment is that tier for PIN purposes, whatever its own role.
func TestManagerCannotResetPINOfAdminAssignedRow(t *testing.T) {
	client := openSQLiteTestClient(t, "secsetpinassign")
	h := NewPINAuthHandler(zap.NewNop(), client, []byte("s"), nil, "", "")
	tid := uuid.New()
	seedTenant(t, client, tid)
	outlet := seedOutlet(t, client, tid, "A")
	managerClaims := secClaims(tid, uuid.New(), "https://sso.example", false, "manager")
	adminClaims := secClaims(tid, uuid.New(), "https://sso.example", false, "admin")

	row := seedStaffMember(t, client, tid, "cashier", outlet.ID)
	seedAdminAssignment(t, client, tid, row.UserID, "admin")

	setPIN := func(claims *authclient.Claims) int {
		rec := httptest.NewRecorder()
		h.SetPIN(rec, secRequest(t, http.MethodPost, tid,
			map[string]any{"user_id": row.UserID.String(), "pin": "4829"}, claims, nil))
		return rec.Code
	}
	if code := setPIN(managerClaims); code != http.StatusForbidden {
		t.Fatalf("manager resetting the PIN of a cashier row with an admin assignment: status = %d; want 403", code)
	}
	if got := client.StaffMember.GetX(context.Background(), row.ID); got.PinHash != nil {
		t.Fatal("a refused PIN reset must not write a hash")
	}
	if code := setPIN(adminClaims); code != http.StatusNoContent {
		t.Fatalf("admin resetting that PIN: status = %d; want 204", code)
	}

	// The weak-PIN refusal follows the same effective tier: no "1234" on an admin-assigned row.
	rec := httptest.NewRecorder()
	h.SetPIN(rec, secRequest(t, http.MethodPost, tid,
		map[string]any{"user_id": row.UserID.String(), "pin": "1234"}, adminClaims, nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("weak PIN on an admin-assigned row: status = %d; want 400", rec.Code)
	}
}

// TestManagerCannotEditOrDeactivateAdminAssignedRow: UpdateStaff and DeactivateStaff use the
// same effective tier as SetPIN, so a manager cannot rename or deactivate a cashier row whose
// person holds an admin POS role assignment. An admin still can.
func TestManagerCannotEditOrDeactivateAdminAssignedRow(t *testing.T) {
	t.Setenv("AUTH_API_URL", "")
	h, client := newStaffTestHandler(t)
	tid := uuid.New()
	seedTenant(t, client, tid)
	outlet := seedOutlet(t, client, tid, "A")
	manager := secClaims(tid, uuid.New(), "https://sso.example", false, "manager")
	admin := secClaims(tid, uuid.New(), "https://sso.example", false, "admin")
	row := seedStaffMember(t, client, tid, "cashier", outlet.ID)
	seedAdminAssignment(t, client, tid, row.UserID, "admin")
	params := map[string]string{"staffID": row.ID.String()}

	rec := httptest.NewRecorder()
	h.UpdateStaff(rec, secRequest(t, http.MethodPatch, tid, map[string]any{"name": "Renamed"}, manager, params))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("manager renaming an admin-assigned row: status = %d; want 403", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.DeactivateStaff(rec, secRequest(t, http.MethodPost, tid, nil, manager, params))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("manager deactivating an admin-assigned row: status = %d; want 403", rec.Code)
	}
	if got := client.StaffMember.GetX(context.Background(), row.ID); got.Name != row.Name || !got.IsActive {
		t.Fatal("a refused edit or deactivation must change nothing")
	}

	rec = httptest.NewRecorder()
	h.UpdateStaff(rec, secRequest(t, http.MethodPatch, tid, map[string]any{"name": "Renamed"}, admin, params))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("admin renaming that row: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// A plain cashier row is still a manager's to edit.
	plain := seedStaffMember(t, client, tid, "cashier", outlet.ID)
	rec = httptest.NewRecorder()
	h.UpdateStaff(rec, secRequest(t, http.MethodPatch, tid, map[string]any{"name": "Fine"}, manager,
		map[string]string{"staffID": plain.ID.String()}))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("manager renaming a plain cashier: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestRoleAssignmentRaiseClearsPIN locks in I-1: a POS role assignment that puts the person in a
// higher tier than their row role and every assignment they already hold clears their staff
// row's PIN in that tenant only. It covers rbac.Service.AssignRole and AssignRoleByCode (the
// JIT path).
func TestRoleAssignmentRaiseClearsPIN(t *testing.T) {
	client := openSQLiteTestClient(t, "secassignraise")
	ctx := context.Background()
	svc := rbac.NewService(rbac.NewEntRepository(client), zap.NewNop())
	tidA, tidB := uuid.New(), uuid.New()
	seedTenant(t, client, tidA)
	seedTenant(t, client, tidB)
	outletA := seedOutlet(t, client, tidA, "A")
	outletB := seedOutlet(t, client, tidB, "B")
	role := func(tid uuid.UUID, code string) *ent.POSRoleV2 {
		return client.POSRoleV2.Create().SetTenantID(tid).SetRoleCode(code).SetName(code).SaveX(ctx)
	}
	waiterA, managerA, adminA := role(tidA, "waiter"), role(tidA, "manager"), role(tidA, "admin")
	role(tidA, "owner")

	hash, _ := bcrypt.GenerateFromPassword([]byte("4829"), bcrypt.MinCost)
	withPIN := func(tid, uid, outletID uuid.UUID) *ent.StaffMember {
		m := client.StaffMember.Create().SetTenantID(tid).SetUserID(uid).SetName("Staff").SetRole("cashier").
			SetPinHash(string(hash)).SetPinFastHash("fast-" + uuid.NewString()).SaveX(ctx)
		client.StaffOutlet.Create().SetTenantID(tid).SetStaffMemberID(m.ID).SetOutletID(outletID).SetIsHomeOutlet(true).ExecX(ctx)
		return m
	}
	user := func(tid uuid.UUID) uuid.UUID {
		id := uuid.New()
		client.User.Create().SetID(id).SetAuthServiceUserID(id).SetTenantID(tid).
			SetEmail(id.String() + "@example.com").SetFullName("Staff").ExecX(ctx)
		return id
	}
	hasPIN := func(m *ent.StaffMember) bool { return client.StaffMember.GetX(ctx, m.ID).PinHash != nil }
	assign := func(uid uuid.UUID, r *ent.POSRoleV2) {
		if err := svc.AssignRole(ctx, tidA, uid, r.ID, uuid.New()); err != nil {
			t.Fatalf("assign %s: %v", r.RoleCode, err)
		}
	}
	reset := func(m *ent.StaffMember) {
		client.StaffMember.UpdateOne(m).SetPinHash(string(hash)).SetPinFastHash("fast-" + uuid.NewString()).ExecX(ctx)
	}

	// The same person has a row in tenant A and in tenant B.
	person := user(tidA)
	rowA := withPIN(tidA, person, outletA.ID)
	rowB := withPIN(tidB, person, outletB.ID)

	assign(person, waiterA)
	if !hasPIN(rowA) {
		t.Fatal("a same-tier assignment (waiter) must keep the PIN")
	}
	assign(person, managerA)
	if hasPIN(rowA) {
		t.Fatal("a manager assignment on a cashier row must clear the PIN")
	}
	if !hasPIN(rowB) {
		t.Fatal("an assignment in tenant A must never touch the person's row in tenant B")
	}
	reset(rowA)
	assign(person, adminA)
	if hasPIN(rowA) {
		t.Fatal("an admin assignment above an existing manager assignment must clear the PIN")
	}

	// Already admin by assignment: another admin-tier role is not a raise.
	reset(rowA)
	if err := svc.AssignRoleByCode(ctx, tidA, person, uuid.New(), "owner"); err != nil {
		t.Fatal(err)
	}
	if !hasPIN(rowA) {
		t.Fatal("an owner assignment for someone already holding admin is not a raise")
	}

	// The JIT path (AssignRoleByCode) raises and clears like AssignRole.
	other := user(tidA)
	otherRow := withPIN(tidA, other, outletA.ID)
	if err := svc.AssignRoleByCode(ctx, tidA, other, uuid.New(), "admin"); err != nil {
		t.Fatal(err)
	}
	if hasPIN(otherRow) {
		t.Fatal("AssignRoleByCode raising to admin must clear the PIN")
	}
}

// TestCardTokenBoundToTenant locks in I-2: a manager card works only on the tenant it was minted
// for and only from the card issuer, so it cannot be replayed into another tenant.
func TestCardTokenBoundToTenant(t *testing.T) {
	secret := []byte("terminal-secret")
	staffUser, tidA, tidB := uuid.New(), uuid.New(), uuid.New()
	token, err := issueCardToken(staffUser, tidA, secret)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := verifyCardToken(token, tidA, secret); !ok || got != staffUser {
		t.Fatal("a card must verify on its own tenant")
	}
	if _, ok := verifyCardToken(token, tidB, secret); ok {
		t.Fatal("a card minted on tenant A must not verify on tenant B")
	}

	forged, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, cardClaims{
		StaffUser: staffUser.String(), Purpose: "manager-card",
		RegisteredClaims: jwt.RegisteredClaims{Issuer: "pos-terminal", Subject: tidA.String(),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString(secret)
	if _, ok := verifyCardToken(forged, tidA, secret); ok {
		t.Fatal("a token from another issuer must not verify as a card")
	}

	// End to end: StepUpByCard on tenant B refuses tenant A's card before any lookup.
	client := openSQLiteTestClient(t, "seccard")
	h := NewPINAuthHandler(zap.NewNop(), client, secret, nil, "", "")
	rec := httptest.NewRecorder()
	h.StepUpByCard(rec, secRequest(t, http.MethodPost, tidB,
		map[string]any{"action": "void", "card_token": token, "outlet_id": uuid.NewString()}, nil, nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant card replay: status = %d, body = %s; want 403", rec.Code, rec.Body.String())
	}
}

// TestPINProfileHidesUserIDAndRole locks in S2-11: the public PIN profile list carries the staff
// id, display name, initials and has_pin, never the auth user id or the role.
func TestPINProfileHidesUserIDAndRole(t *testing.T) {
	client := openSQLiteTestClient(t, "secprofile")
	h := NewPINAuthHandler(zap.NewNop(), client, []byte("s"), nil, "", "")
	tid := uuid.New()
	seedTenant(t, client, tid)
	outlet := seedOutlet(t, client, tid, "A")
	admin := seedStaffMember(t, client, tid, "admin", outlet.ID)
	client.StaffMember.UpdateOne(admin).SetName("grace wanjiru njeri").ExecX(context.Background())

	for name, claims := range map[string]*authclient.Claims{
		"public":    nil,
		"signed in": secClaims(tid, uuid.New(), terminalIssuer, false, "admin"),
	} {
		rec := httptest.NewRecorder()
		h.StaffProfiles(rec, secRequest(t, http.MethodGet, tid, nil, claims, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %s", name, rec.Code, rec.Body.String())
		}
		var out struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Data) != 1 {
			t.Fatalf("%s: decode: %v, body = %s", name, err, rec.Body.String())
		}
		p := out.Data[0]
		for _, key := range []string{"user_id", "role", "roles", "outlet_id"} {
			if _, ok := p[key]; ok {
				t.Fatalf("%s: profile must not carry %q, got %v", name, key, p)
			}
		}
		if p["staff_id"] != admin.ID.String() || p["name"] != "grace wanjiru njeri" || p["initials"] != "GW" ||
			p["tenant_id"] != tid.String() || p["has_pin"] != false {
			t.Fatalf("%s: unexpected profile %v", name, p)
		}
	}
}

// TestPINLoginByStaffID locks in that the PIN login accepts the opaque staff id the picker now
// holds, and that its response names the staff id so the device can key its offline cache.
func TestPINLoginByStaffID(t *testing.T) {
	t.Setenv("AUTH_API_URL", "")
	client := openSQLiteTestClient(t, "secloginstaff")
	h := NewPINAuthHandler(zap.NewNop(), client, []byte("terminal-secret"), nil, "", "")
	tid := uuid.New()
	seedTenant(t, client, tid)
	outlet := seedOutlet(t, client, tid, "A")
	ctx := context.Background()
	staff := seedStaffMember(t, client, tid, "cashier", outlet.ID)
	hash, _ := bcrypt.GenerateFromPassword([]byte("4829"), bcrypt.MinCost)
	client.StaffMember.UpdateOne(staff).SetPinHash(string(hash)).ExecX(ctx)

	login := func(body map[string]any) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.Login(rec, secRequest(t, http.MethodPost, tid, body, nil, nil))
		return rec
	}

	rec := login(map[string]any{"staff_id": staff.ID.String(), "pin": "4829"})
	if rec.Code != http.StatusOK {
		t.Fatalf("login by staff_id: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		AccessToken string         `json:"access_token"`
		User        map[string]any `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.AccessToken == "" {
		t.Fatalf("decode: %v, body = %s", err, rec.Body.String())
	}
	if out.User["staff_id"] != staff.ID.String() {
		t.Fatalf("login response must name the staff id, got %v", out.User)
	}

	if rec := login(map[string]any{"staff_id": staff.ID.String(), "pin": "0000"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong PIN by staff_id: status = %d; want 401", rec.Code)
	}
	if rec := login(map[string]any{"staff_id": uuid.NewString(), "pin": "4829"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown staff_id: status = %d; want 401", rec.Code)
	}
	// A staff id from another tenant never signs in here.
	other := uuid.New()
	seedTenant(t, client, other)
	foreign := seedStaffMember(t, client, other, "cashier", seedOutlet(t, client, other, "B").ID)
	client.StaffMember.UpdateOne(foreign).SetPinHash(string(hash)).ExecX(ctx)
	if rec := login(map[string]any{"staff_id": foreign.ID.String(), "pin": "4829"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("another tenant's staff_id: status = %d; want 401", rec.Code)
	}
}
