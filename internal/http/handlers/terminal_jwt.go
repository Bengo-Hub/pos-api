package handlers

import (
	"context"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

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
// extra API round-trip.
func issueTerminalJWT(member *ent.StaffMember, tenantID uuid.UUID, secret []byte, client *ent.Client, ctx context.Context) (string, error) {
	permissions := resolveRolePermissions(ctx, client, tenantID, member.Role)

	// Load outlet to include use_case and is_hq in terminal JWT claims
	outletCode := ""
	outletUseCase := "hospitality" // safe default
	isHQ := false
	outlet, err := client.Outlet.Get(ctx, member.OutletID)
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
		OutletID:      member.OutletID.String(),
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
