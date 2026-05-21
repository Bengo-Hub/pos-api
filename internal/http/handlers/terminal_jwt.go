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
)

// terminalClaims are embedded in short-lived JWTs issued to POS terminals after PIN login.
// Mirrors the SSO JWT shape so pos-ui can use the same claim parsing path.
type terminalClaims struct {
	UserID         string   `json:"user_id"`
	TenantID       string   `json:"tenant_id"`
	OutletID       string   `json:"outlet_id"`
	OutletCode     string   `json:"outlet_code"`
	OutletUseCase  string   `json:"outlet_use_case"`
	IsHQUser       bool     `json:"is_hq_user"`
	Name           string   `json:"name"`
	Role           string   `json:"role"`
	Permissions    []string `json:"permissions"`
	jwt.RegisteredClaims
}

// issueTerminalJWT signs a 4-hour HMAC-SHA256 JWT for terminal PIN sessions.
// It resolves permissions from the tenant's POSRoleV2 for the staff member's role
// and embeds outlet_use_case + is_hq_user so pos-ui can gate modules without an
// extra API round-trip. sessionOutletID is the outlet the terminal selected at login
// (may differ from member.OutletID which is the staff member's home outlet).
func issueTerminalJWT(member *ent.StaffMember, tenantID uuid.UUID, sessionOutletID uuid.UUID, secret []byte, client *ent.Client, ctx context.Context) (string, error) {
	permissions := resolveRolePermissions(ctx, client, tenantID, member.Role)

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
		UserID:        member.UserID.String(),
		TenantID:      tenantID.String(),
		OutletID:      sessionOutletID.String(),
		OutletCode:    outletCode,
		OutletUseCase: outletUseCase,
		IsHQUser:      isHQ,
		Name:          member.Name,
		Role:          member.Role,
		Permissions:   permissions,
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
		OutletID:      tc.OutletID,
		OutletCode:    tc.OutletCode,
		OutletUseCase: tc.OutletUseCase,
		IsHQUser:      tc.IsHQUser,
		Roles:         []string{tc.Role},
		Permissions:   tc.Permissions,
		// SubscriptionStatus left empty → SubscriptionGate treats "" as ACTIVE/allowed
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

// resolveRolePermissions looks up the POSRoleV2 for the given tenantID and roleCode,
// loads its permissions via eager-load, and returns the permission codes.
// Returns an empty slice if the role is not found or an error occurs.
func resolveRolePermissions(ctx context.Context, client *ent.Client, tenantID uuid.UUID, roleCode string) []string {
	role, err := client.POSRoleV2.Query().
		Where(
			posrolev2.TenantID(tenantID),
			posrolev2.RoleCode(roleCode),
		).
		WithPermissions().
		Only(ctx)
	if err != nil {
		// Role not found or error — fall back to empty permissions
		return []string{}
	}

	codes := make([]string, 0, len(role.Edges.Permissions))
	for _, p := range role.Edges.Permissions {
		codes = append(codes, p.PermissionCode)
	}
	return codes
}
