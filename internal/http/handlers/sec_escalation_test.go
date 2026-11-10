package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/bengobox/pos-service/internal/ent"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
	outletmw "github.com/bengobox/pos-service/internal/http/middleware"
)

// secClaims builds request claims the way the auth middlewares would.
func secClaims(tid uuid.UUID, subject uuid.UUID, issuer string, platformOwner bool, roles ...string) *authclient.Claims {
	return &authclient.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: subject.String(), Issuer: issuer},
		TenantID:         tid.String(),
		Roles:            roles,
		IsPlatformOwner:  platformOwner,
	}
}

// secRequest builds a request carrying the tenant and (optionally) claims in its context.
func secRequest(t *testing.T, method string, tid uuid.UUID, body any, claims *authclient.Claims, urlParams map[string]string) *http.Request {
	t.Helper()
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, "/", bytes.NewReader(buf))
	rctx := chi.NewRouteContext()
	for k, v := range urlParams {
		rctx.URLParams.Add(k, v)
	}
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = httpware.WithTenantID(ctx, tid.String())
	if claims != nil {
		ctx = authclient.ContextWithClaims(ctx, claims)
	}
	return req.WithContext(ctx)
}

// fakeAuthAPI stands in for auth-api's S2S endpoints used by pos: platform-owner lookup,
// tenant membership lookup and member provisioning.
type fakeAuthAPI struct {
	srv            *httptest.Server
	platformOwners map[uuid.UUID]bool
	members        map[string]bool // tenant/user
	provisioned    uuid.UUID
	memberLookups  atomic.Int32
}

func newFakeAuthAPI(t *testing.T) *fakeAuthAPI {
	t.Helper()
	f := &fakeAuthAPI{platformOwners: map[uuid.UUID]bool{}, members: map[string]bool{}, provisioned: uuid.New()}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/platform-owner"):
			uid, _ := uuid.Parse(parts[len(parts)-2])
			_ = json.NewEncoder(w).Encode(map[string]any{"is_platform_owner": f.platformOwners[uid]})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/outlet-limit"):
			f.memberLookups.Add(1)
			// /api/v1/s2s/tenants/{tenant}/members/{user}/outlet-limit
			key := parts[len(parts)-4] + "/" + parts[len(parts)-2]
			if !f.members[key] {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"member not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"outlet_ids": []string{}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/members"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"user_id": f.provisioned.String()})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	t.Setenv("AUTH_API_URL", f.srv.URL)
	return f
}

// TestPINTokenPlatformOwnerOnlyOnPlatformTenant locks in the C1 fix: auth-api's "is this user a
// platform owner" answer is global, so a PIN token may carry platform owner only on the
// configured platform tenant. A tenant admin who points a staff row at a platform admin's user
// id on their own tenant gets an ordinary tenant token.
func TestPINTokenPlatformOwnerOnlyOnPlatformTenant(t *testing.T) {
	client := openSQLiteTestClient(t, "secpo")
	ctx := context.Background()
	secret := []byte("terminal-secret")
	auth := newFakeAuthAPI(t)

	platform := seedTenant(t, client, uuid.New())
	other := seedTenant(t, client, uuid.New())
	other2 := seedTenant(t, client, uuid.New())
	platformAdminUser := uuid.New()
	platformCashierUser := uuid.New()
	auth.platformOwners[platformAdminUser] = true
	auth.platformOwners[platformCashierUser] = true

	mkStaff := func(tid, uid uuid.UUID, role string) *ent.StaffMember {
		m, err := client.StaffMember.Create().SetTenantID(tid).SetUserID(uid).
			SetName("Staff " + role).SetRole(role).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	// Staff rows a tenant admin pointed at the platform admin's auth user id.
	hijacked := mkStaff(other.ID, platformAdminUser, "admin")
	hijackedOwner := mkStaff(other2.ID, platformAdminUser, "owner")
	genuine := mkStaff(platform.ID, platformAdminUser, "admin")
	platformCashier := mkStaff(platform.ID, platformCashierUser, "cashier")

	h := NewPINAuthHandler(zap.NewNop(), client, secret, nil, auth.srv.URL, "internal-key")
	h.SetPlatformTenantID(platform.ID.String())

	mint := func(t *testing.T, tid uuid.UUID, m *ent.StaffMember) *authclient.Claims {
		t.Helper()
		subEnt := h.sessionEntitlements(ctx, tid, m)
		token, _, err := issueTerminalJWT(m, tid, uuid.Nil, secret, client, ctx, subEnt)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		tc, err := validateTerminalJWT(token, secret)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		return terminalToAuthClaims(tc)
	}

	if mint(t, other.ID, hijacked).IsPlatformOwner {
		t.Fatal("a staff row on a non-platform tenant pointing at a platform admin must not mint a platform-owner token")
	}
	if mint(t, other2.ID, hijackedOwner).IsPlatformOwner {
		t.Fatal("role alias owner on a non-platform tenant must not mint a platform-owner token")
	}
	if mint(t, platform.ID, platformCashier).IsPlatformOwner {
		t.Fatal("a non-admin staff row must never carry platform owner")
	}
	if !mint(t, platform.ID, genuine).IsPlatformOwner {
		t.Fatal("a platform admin signed in on the platform tenant keeps platform owner")
	}

	// Unset or invalid platform tenant: nobody is a platform owner on a PIN token.
	h.SetPlatformTenantID("")
	if mint(t, platform.ID, genuine).IsPlatformOwner {
		t.Fatal("without PLATFORM_TENANT_ID no PIN token may carry platform owner")
	}
	h.SetPlatformTenantID("not-a-uuid")
	if mint(t, platform.ID, genuine).IsPlatformOwner {
		t.Fatal("an invalid PLATFORM_TENANT_ID must disable platform owner on PIN tokens")
	}
}

// TestCreateStaffRejectsForeignUserID locks in that CreateStaff no longer trusts a client-supplied
// user_id: it must be a member of the tenant in auth-api, or the caller an SSO platform owner.
func TestCreateStaffRejectsForeignUserID(t *testing.T) {
	auth := newFakeAuthAPI(t)
	client := openSQLiteTestClient(t, "secforeign")
	h := NewStaffHandler(zap.NewNop(), client, auth.srv.URL, "internal-key")
	tid := uuid.New()
	seedTenant(t, client, tid)
	outlet := seedOutlet(t, client, tid, "A")
	tenantAdmin := secClaims(tid, uuid.New(), "https://sso.example", false, "admin")

	foreign := uuid.New()
	create := func(claims *authclient.Claims, body map[string]any) *httptest.ResponseRecorder {
		body["outlet_id"] = outlet.ID.String()
		body["name"] = "New Staff"
		if _, ok := body["role"]; !ok {
			body["role"] = "cashier"
		}
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

	// Foreign user id, no email: refused, nothing written.
	if rec := create(tenantAdmin, map[string]any{"user_id": foreign.String(), "role": "admin", "pin": "4829"}); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign user_id: status = %d, body = %s; want 403", rec.Code, rec.Body.String())
	}
	if rowsFor(foreign) != 0 {
		t.Fatal("a staff row must never point at a user who is not a member of the tenant")
	}

	// Foreign user id plus an email: the id is ignored and the user is provisioned by email.
	if rec := create(tenantAdmin, map[string]any{"user_id": foreign.String(), "email": "new@example.com"}); rec.Code != http.StatusCreated {
		t.Fatalf("foreign user_id with email: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rowsFor(foreign) != 0 || rowsFor(auth.provisioned) != 1 {
		t.Fatal("with an email the staff row must use the id auth-api provisions, not the foreign one")
	}

	// A terminal token claiming platform owner is not an SSO platform owner.
	terminalPO := secClaims(tid, uuid.New(), terminalIssuer, true, "admin")
	if rec := create(terminalPO, map[string]any{"user_id": foreign.String()}); rec.Code != http.StatusForbidden {
		t.Fatalf("terminal platform owner with foreign user_id: status = %d; want 403", rec.Code)
	}

	// A real member of the tenant is accepted.
	memberUser := uuid.New()
	auth.members[tid.String()+"/"+memberUser.String()] = true
	if rec := create(tenantAdmin, map[string]any{"user_id": memberUser.String()}); rec.Code != http.StatusCreated {
		t.Fatalf("member user_id: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rowsFor(memberUser) != 1 {
		t.Fatal("a member's user_id must be honoured")
	}

	// An SSO platform owner may link any user without the membership lookup.
	before := auth.memberLookups.Load()
	ssoPO := secClaims(uuid.New(), uuid.New(), "https://sso.example", true, "superuser")
	anyUser := uuid.New()
	if rec := create(ssoPO, map[string]any{"user_id": anyUser.String()}); rec.Code != http.StatusCreated {
		t.Fatalf("SSO platform owner: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if auth.memberLookups.Load() != before || rowsFor(anyUser) != 1 {
		t.Fatal("an SSO platform owner's user_id must be honoured without a membership lookup")
	}
}

// TestManagerCannotGrantOwnerOrSuperuser locks in the canonicalised role guard: a caller below
// admin cannot create, promote into or edit any admin or manager-tier role, whatever the alias.
func TestManagerCannotGrantOwnerOrSuperuser(t *testing.T) {
	t.Setenv("AUTH_API_URL", "")
	h, client := newStaffTestHandler(t)
	tid := uuid.New()
	seedTenant(t, client, tid)
	outlet := seedOutlet(t, client, tid, "A")

	callers := map[string]*authclient.Claims{
		"manager":       secClaims(tid, uuid.New(), "https://sso.example", false, "manager"),
		"store_manager": secClaims(tid, uuid.New(), "https://sso.example", false, "store_manager"),
		"cashier":       secClaims(tid, uuid.New(), "https://sso.example", false, "cashier"),
	}
	privileged := []string{"owner", "superuser", "super_admin", "pos_admin", " Admin ", "ADMIN", "manager", "store_manager", "supervisor"}

	for name, caller := range callers {
		for _, role := range privileged {
			rec := httptest.NewRecorder()
			body := map[string]any{"email": "x@example.com", "name": "X", "role": role, "outlet_id": outlet.ID.String()}
			h.CreateStaff(rec, secRequest(t, http.MethodPost, tid, body, caller, nil))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s creating %q: status = %d; want 403", name, role, rec.Code)
			}

			cashier := seedStaffMember(t, client, tid, "cashier", outlet.ID)
			rec = httptest.NewRecorder()
			h.UpdateStaff(rec, secRequest(t, http.MethodPatch, tid, map[string]any{"role": role}, caller,
				map[string]string{"staffID": cashier.ID.String()}))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s promoting a cashier to %q: status = %d; want 403", name, role, rec.Code)
			}
			if got, _ := client.StaffMember.Get(context.Background(), cashier.ID); got.Role != "cashier" {
				t.Fatalf("%s promoting to %q changed the role to %q", name, role, got.Role)
			}
		}

		owner := seedStaffMember(t, client, tid, "owner", outlet.ID)
		rec := httptest.NewRecorder()
		h.UpdateStaff(rec, secRequest(t, http.MethodPatch, tid, map[string]any{"name": "Renamed"}, caller,
			map[string]string{"staffID": owner.ID.String()}))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s editing an owner: status = %d; want 403", name, rec.Code)
		}
		rec = httptest.NewRecorder()
		h.DeactivateStaff(rec, secRequest(t, http.MethodPost, tid, nil, caller,
			map[string]string{"staffID": owner.ID.String()}))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s deactivating an owner: status = %d; want 403", name, rec.Code)
		}
	}

	// An admin may still promote.
	admin := secClaims(tid, uuid.New(), "https://sso.example", false, "admin")
	cashier := seedStaffMember(t, client, tid, "cashier", outlet.ID)
	rec := httptest.NewRecorder()
	h.UpdateStaff(rec, secRequest(t, http.MethodPatch, tid, map[string]any{"role": "manager"}, admin,
		map[string]string{"staffID": cashier.ID.String()}))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("admin promoting to manager: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestManagerCannotResetAdminPIN locks in the SetPIN guard: resetting a PIN is signing in as that
// person, so only an admin-level caller may reset an admin or manager-tier PIN.
func TestManagerCannotResetAdminPIN(t *testing.T) {
	client := openSQLiteTestClient(t, "secsetpin")
	h := NewPINAuthHandler(zap.NewNop(), client, []byte("s"), nil, "", "")
	tid := uuid.New()
	seedTenant(t, client, tid)
	outlet := seedOutlet(t, client, tid, "A")
	manager := seedStaffMember(t, client, tid, "manager", outlet.ID)
	managerClaims := secClaims(tid, manager.UserID, "https://sso.example", false, "manager")

	setPIN := func(claims *authclient.Claims, target *ent.StaffMember) int {
		rec := httptest.NewRecorder()
		h.SetPIN(rec, secRequest(t, http.MethodPost, tid,
			map[string]any{"user_id": target.UserID.String(), "pin": "4829"}, claims, nil))
		return rec.Code
	}

	for _, role := range []string{"admin", "owner", "superuser", " Super_Admin ", "store_manager"} {
		target := seedStaffMember(t, client, tid, role, outlet.ID)
		if code := setPIN(managerClaims, target); code != http.StatusForbidden {
			t.Fatalf("manager resetting %q PIN: status = %d; want 403", role, code)
		}
		if got, _ := client.StaffMember.Get(context.Background(), target.ID); got.PinHash != nil {
			t.Fatalf("manager resetting %q PIN must not write a hash", role)
		}
	}

	cashier := seedStaffMember(t, client, tid, "cashier", outlet.ID)
	if code := setPIN(managerClaims, cashier); code != http.StatusNoContent {
		t.Fatalf("manager resetting a cashier PIN: status = %d; want 204", code)
	}
	if code := setPIN(managerClaims, manager); code != http.StatusNoContent {
		t.Fatalf("manager changing their own PIN: status = %d; want 204", code)
	}
	admin := seedStaffMember(t, client, tid, "admin", outlet.ID)
	adminClaims := secClaims(tid, uuid.New(), "https://sso.example", false, "admin")
	if code := setPIN(adminClaims, admin); code != http.StatusNoContent {
		t.Fatalf("admin resetting an admin PIN: status = %d; want 204", code)
	}
}

// TestPublicStaffListHidesUserIDs locks in that the public PIN picker list no longer hands out
// auth user ids and roles, while a caller signed in to the tenant still gets them.
func TestPublicStaffListHidesUserIDs(t *testing.T) {
	client := openSQLiteTestClient(t, "secliststaff")
	h := NewPINAuthHandler(zap.NewNop(), client, []byte("s"), nil, "", "")
	tid := uuid.New()
	seedTenant(t, client, tid)
	outlet := seedOutlet(t, client, tid, "A")
	admin := seedStaffMember(t, client, tid, "admin", outlet.ID)

	list := func(claims *authclient.Claims) []map[string]any {
		rec := httptest.NewRecorder()
		h.ListStaff(rec, secRequest(t, http.MethodGet, tid, nil, claims, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("ListStaff: status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Data) != 1 {
			t.Fatalf("decode: %v, body = %s", err, rec.Body.String())
		}
		return out.Data
	}

	public := list(nil)[0]
	if _, ok := public["user_id"]; ok {
		t.Fatal("public staff list must not return user_id")
	}
	if _, ok := public["role"]; ok {
		t.Fatal("public staff list must not return role")
	}
	if public["id"] != admin.ID.String() || public["name"] != admin.Name {
		t.Fatalf("public staff list must return the opaque staff id and name, got %v", public)
	}

	otherTenant := list(secClaims(uuid.New(), uuid.New(), "https://sso.example", false, "admin"))[0]
	if _, ok := otherTenant["user_id"]; ok {
		t.Fatal("a token for another tenant must get the public shape")
	}

	signedIn := list(secClaims(tid, uuid.New(), terminalIssuer, false, "cashier"))[0]
	if signedIn["user_id"] != admin.UserID.String() || signedIn["role"] != "admin" {
		t.Fatalf("a caller signed in to the tenant keeps user_id and role, got %v", signedIn)
	}
}

// TestRepairTokenScopedToTenant locks in that the maintenance override token is not a platform
// owner: it passes its own tenant's maintenance gate and is refused on every other tenant.
func TestRepairTokenScopedToTenant(t *testing.T) {
	client := openSQLiteTestClient(t, "secrepair")
	ctx := context.Background()
	secret := []byte("terminal-secret")
	h := NewPINAuthHandler(zap.NewNop(), client, secret, nil, "", "")

	now := time.Now()
	underRepair, err := client.Tenant.Create().SetID(uuid.New()).SetName("Repair").SetSlug("repair-" + uuid.NewString()[:8]).
		SetMaintenanceStartsAt(now.Add(-time.Hour)).SetMaintenanceEndsAt(now.Add(time.Hour)).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	otherTenant := seedTenant(t, client, uuid.New())
	outlet := seedOutlet(t, client, underRepair.ID, "R")

	token, _, err := issuePlatformRepairOverrideJWT(underRepair.ID, underRepair.Slug, outlet.ID, secret, client, ctx)
	if err != nil {
		t.Fatal(err)
	}
	tc, err := validateTerminalJWT(token, secret)
	if err != nil {
		t.Fatal(err)
	}
	if tc.IsPlatformOwner || terminalToAuthClaims(tc).IsPlatformOwner {
		t.Fatal("the repair token must not carry platform owner")
	}
	if !tc.PlatformRepair || tc.TenantID != underRepair.ID.String() {
		t.Fatalf("the repair token must be a repair session for its tenant, got repair=%v tenant=%s", tc.PlatformRepair, tc.TenantID)
	}

	// Same middleware order as the router: auth, maintenance gate, then TenantV2 per tenant.
	r := chi.NewRouter()
	r.Use(h.RequireAnyAuth(nil))
	r.Use(outletmw.RequireNotUnderMaintenance(client))
	r.Route("/{tenantID}", func(tr chi.Router) {
		tr.Use(httpware.TenantV2(httpware.TenantConfig{
			ClaimsExtractor: func(ctx context.Context) (string, string, bool, bool) {
				c, ok := authclient.ClaimsFromContext(ctx)
				if !ok {
					return "", "", false, false
				}
				return c.TenantID, c.GetTenantSlug(), c.IsPlatformOwner, true
			},
			URLParamFunc: chi.URLParam,
			URLParamName: "tenantID",
			Required:     true,
		}))
		tr.Get("/ping", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	})
	call := func(tok string, tid uuid.UUID) int {
		req := httptest.NewRequest(http.MethodGet, "/"+tid.String()+"/ping", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := call(token, underRepair.ID); code != http.StatusOK {
		t.Fatalf("repair token on its own tenant during maintenance: status = %d; want 200", code)
	}
	if code := call(token, otherTenant.ID); code != http.StatusForbidden {
		t.Fatalf("repair token on another tenant: status = %d; want 403", code)
	}

	// An ordinary admin PIN token on the same tenant is still blocked by the window.
	pinHash, _ := bcrypt.GenerateFromPassword([]byte("4829"), bcrypt.MinCost)
	staff, err := client.StaffMember.Create().SetTenantID(underRepair.ID).SetUserID(uuid.New()).
		SetName("Admin").SetRole("admin").SetPinHash(string(pinHash)).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adminToken, _, err := issueTerminalJWT(staff, underRepair.ID, outlet.ID, secret, client, ctx, terminalEntitlements{})
	if err != nil {
		t.Fatal(err)
	}
	if code := call(adminToken, underRepair.ID); code != http.StatusServiceUnavailable {
		t.Fatalf("ordinary admin during maintenance: status = %d; want 503", code)
	}
}
