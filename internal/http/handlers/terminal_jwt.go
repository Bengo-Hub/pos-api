package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posrolev2"
	"github.com/bengobox/pos-service/internal/ent/posuserroleassignment"
)

// terminalClaims are embedded in short-lived JWTs issued to POS terminals after PIN login.
// Mirrors the SSO JWT shape so pos-ui can use the same claim parsing path AND so the
// subscription gate (SubscriptionGate / RequireFeature / CheckStructuralLimit) sees the
// same entitlements + bypass flags it would for an SSO session. Without these, every PIN
// session was treated as having zero features (→ 403 on feature-gated routes) and demo /
// platform-owner tenants were not exempted.
type terminalClaims struct {
	UserID        string   `json:"user_id"`
	TenantID      string   `json:"tenant_id"`
	TenantSlug    string   `json:"tenant_slug"`
	OutletID      string   `json:"outlet_id"`
	OutletCode    string   `json:"outlet_code"`
	OutletUseCase string   `json:"outlet_use_case"`
	IsHQUser      bool     `json:"is_hq_user"`
	Name          string   `json:"name"`
	Role          string   `json:"role"`
	Permissions   []string `json:"permissions"`
	// Subscription + bypass claims (mirror the shared authclient.Claims tags).
	IsPlatformOwner      bool           `json:"is_platform_owner,omitempty"`
	IsDemo               bool           `json:"is_demo,omitempty"`
	BillingMode          string         `json:"billing_mode,omitempty"`
	SubscriptionStatus   string         `json:"sub_status,omitempty"`
	SubscriptionFeatures []string       `json:"subscription_features,omitempty"`
	SubscriptionLimits   map[string]int `json:"sub_limits,omitempty"`
	jwt.RegisteredClaims
}

// issueTerminalJWT signs a 4-hour HMAC-SHA256 JWT for terminal PIN sessions.
// It resolves permissions from the tenant's POSRoleV2 for the staff member's role
// and embeds outlet_use_case + is_hq_user so pos-ui can gate modules without an
// extra API round-trip. sessionOutletID is the outlet the terminal selected at login
// (may differ from member.OutletID which is the staff member's home outlet).
// terminalEntitlements carries the subscription snapshot + bypass flags resolved at PIN
// login so issueTerminalJWT can embed them. It is built by the Login handler (which has the
// subscriptions client); issueTerminalJWT stays decoupled from the HTTP layer.
type terminalEntitlements struct {
	TenantSlug      string
	IsPlatformOwner bool
	IsDemo          bool
	BillingMode     string
	Status          string
	Features        []string
	Limits          map[string]int
}

func issueTerminalJWT(member *ent.StaffMember, tenantID uuid.UUID, sessionOutletID uuid.UUID, secret []byte, client *ent.Client, ctx context.Context, ent2 terminalEntitlements) (string, error) {
	permissions := resolveEffectivePermissions(ctx, client, tenantID, member.UserID, member.Role, nil)

	// Load outlet to include use_case and is_hq in terminal JWT claims
	outletCode := ""
	outletUseCase := "hospitality" // safe default
	isHQ := false
	outlet, err := client.Outlet.Get(ctx, sessionOutletID)
	if err == nil {
		outletCode = outlet.Code
		if outlet.UseCase != nil {
			outletUseCase = *outlet.UseCase
		}
		isHQ = outlet.IsHq
	}

	now := time.Now()
	claims := terminalClaims{
		UserID:               member.UserID.String(),
		TenantID:             tenantID.String(),
		TenantSlug:           ent2.TenantSlug,
		OutletID:             sessionOutletID.String(),
		OutletCode:           outletCode,
		OutletUseCase:        outletUseCase,
		IsHQUser:             isHQ,
		Name:                 member.Name,
		Role:                 member.Role,
		Permissions:          permissions,
		IsPlatformOwner:      ent2.IsPlatformOwner,
		IsDemo:               ent2.IsDemo,
		BillingMode:          ent2.BillingMode,
		SubscriptionStatus:   ent2.Status,
		SubscriptionFeatures: ent2.Features,
		SubscriptionLimits:   ent2.Limits,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   member.UserID.String(),
			Issuer:    "pos-terminal",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(4 * time.Hour)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(secret)
}

// validateTerminalJWT parses and validates an HMAC-signed terminal JWT.
func validateTerminalJWT(tokenStr string, secret []byte) (*terminalClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &terminalClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return secret, nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*terminalClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid terminal JWT")
	}
	if claims.Issuer != "pos-terminal" {
		return nil, fmt.Errorf("not a terminal JWT")
	}
	return claims, nil
}

// terminalToAuthClaims converts a terminal JWT's claims into the shared authclient.Claims
// format so downstream middleware (TenantV2, SubscriptionGate, RBAC) can use them uniformly.
func terminalToAuthClaims(tc *terminalClaims) *authclient.Claims {
	return &authclient.Claims{
		TenantID:      tc.TenantID,
		TenantSlug:    tc.TenantSlug,
		OutletID:      tc.OutletID,
		OutletCode:    tc.OutletCode,
		OutletUseCase: tc.OutletUseCase,
		IsHQUser:      tc.IsHQUser,
		Roles:         []string{tc.Role},
		Permissions:   tc.Permissions,
		// Carry the subscription snapshot + bypass flags so RequireFeature /
		// CheckStructuralLimit / SubscriptionGate treat a PIN session exactly like an SSO
		// session (and exempt demo / platform-owner tenants).
		IsPlatformOwner:      tc.IsPlatformOwner,
		IsDemo:               tc.IsDemo,
		BillingMode:          tc.BillingMode,
		SubscriptionStatus:   tc.SubscriptionStatus,
		SubscriptionFeatures: tc.SubscriptionFeatures,
		SubscriptionLimits:   tc.SubscriptionLimits,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: tc.Subject,
			Issuer:  tc.Issuer,
		},
	}
}

// RequireAnyAuth returns a middleware that accepts either a terminal PIN JWT (HMAC-SHA256
// signed by pos-api) or a standard SSO JWT. Terminal JWTs are validated first; if they
// fail, the request falls through to the standard RequireAuth middleware.
func (h *PINAuthHandler) RequireAnyAuth(ssoAuth *authclient.AuthMiddleware) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Browser WebSocket / EventSource clients cannot set the Authorization
			// header, so streaming endpoints pass the token as ?access_token=.
			// Promote it into the header so both auth paths below work unchanged.
			if r.Header.Get("Authorization") == "" {
				if qt := r.URL.Query().Get("access_token"); qt != "" {
					r.Header.Set("Authorization", "Bearer "+qt)
				}
			}
			authHeader := r.Header.Get("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
				if tc, err := validateTerminalJWT(tokenStr, h.jwtSecret); err == nil {
					ctx := authclient.ContextWithClaims(r.Context(), terminalToAuthClaims(tc))
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
			// Not a valid terminal JWT — delegate to SSO auth
			if ssoAuth != nil {
				ssoAuth.RequireAuth(next).ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// resolveEffectivePermissions returns the UNION of every permission source a principal holds:
//  1. the staff/system role's grants (StaffMember.Role, or the mapped global role for SSO
//     users with no staff record) — resolveRolePermissions;
//  2. per-user POSUserRoleAssignment rows — the CUSTOM roles a tenant admin creates and
//     assigns via the permission-matrix UI. These were previously ignored by /auth/me and
//     the terminal JWT, so a user holding only a custom role surfaced ZERO permissions and
//     the UI collapsed to dashboard-only even though the role carried full grants;
//  3. tenant/custom POS roles whose role_code matches a raw global JWT role name — covers
//     custom roles assigned on the auth side that the fixed global→POS mapping can't know.
func resolveEffectivePermissions(ctx context.Context, client *ent.Client, tenantID, userID uuid.UUID, roleCode string, globalRoles []string) []string {
	set := map[string]struct{}{}
	add := func(codes []string) {
		for _, c := range codes {
			set[c] = struct{}{}
		}
	}
	add(resolveRolePermissions(ctx, client, tenantID, roleCode))

	// Per-user assignments → their roles' grants (custom roles included).
	if userID != uuid.Nil {
		roleIDs, err := client.POSUserRoleAssignment.Query().
			Where(
				posuserroleassignment.TenantID(tenantID),
				posuserroleassignment.UserID(userID),
			).
			Select(posuserroleassignment.FieldRoleID).
			Strings(ctx)
		if err == nil && len(roleIDs) > 0 {
			ids := make([]uuid.UUID, 0, len(roleIDs))
			for _, s := range roleIDs {
				if id, perr := uuid.Parse(s); perr == nil {
					ids = append(ids, id)
				}
			}
			if len(ids) > 0 {
				roles, rerr := client.POSRoleV2.Query().
					Where(posrolev2.IDIn(ids...)).
					WithPermissions().
					All(ctx)
				if rerr == nil {
					for _, role := range roles {
						for _, p := range role.Edges.Permissions {
							set[p.PermissionCode] = struct{}{}
						}
					}
				}
			}
		}
	}

	// Raw global role names that ARE a POS role code (tenant custom or shared).
	for _, gr := range globalRoles {
		if gr == "" || gr == roleCode {
			continue
		}
		add(resolveRolePermissions(ctx, client, tenantID, gr))
	}

	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	return out
}

// resolveAssignedRoleCodes returns the role codes a user holds via POSUserRoleAssignment
// rows (system + custom roles) — surfaced on /auth/me so the UI can show what's assigned.
func resolveAssignedRoleCodes(ctx context.Context, client *ent.Client, tenantID, userID uuid.UUID) []string {
	codes := []string{}
	if userID == uuid.Nil {
		return codes
	}
	roleIDs, err := client.POSUserRoleAssignment.Query().
		Where(
			posuserroleassignment.TenantID(tenantID),
			posuserroleassignment.UserID(userID),
		).
		Select(posuserroleassignment.FieldRoleID).
		Strings(ctx)
	if err != nil || len(roleIDs) == 0 {
		return codes
	}
	ids := make([]uuid.UUID, 0, len(roleIDs))
	for _, s := range roleIDs {
		if id, perr := uuid.Parse(s); perr == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return codes
	}
	roles, err := client.POSRoleV2.Query().Where(posrolev2.IDIn(ids...)).All(ctx)
	if err != nil {
		return codes
	}
	for _, ro := range roles {
		codes = append(codes, ro.RoleCode)
	}
	return codes
}

// resolveRolePermissions looks up the POSRoleV2 for the given roleCode and returns its permission
// codes. Roles are platform-wide (shared): it matches the shared global/system role (tenant_id NULL)
// OR a tenant-specific custom role of the same code, preferring the tenant-specific override when both
// exist. Returns an empty slice if the role is not found or an error occurs.
func resolveRolePermissions(ctx context.Context, client *ent.Client, tenantID uuid.UUID, roleCode string) []string {
	roles, err := client.POSRoleV2.Query().
		Where(
			posrolev2.RoleCode(roleCode),
			posrolev2.Or(
				posrolev2.TenantID(tenantID),
				posrolev2.TenantIDIsNil(),
			),
		).
		WithPermissions().
		All(ctx)
	if err != nil || len(roles) == 0 {
		// Role not found or error — fall back to empty permissions
		return []string{}
	}

	// Prefer the tenant-specific override; otherwise use the shared global role.
	role := roles[0]
	for _, r := range roles {
		if r.TenantID != nil {
			role = r
			break
		}
	}

	codes := make([]string, 0, len(role.Edges.Permissions))
	for _, p := range role.Edges.Permissions {
		codes = append(codes, p.PermissionCode)
	}
	return codes
}
