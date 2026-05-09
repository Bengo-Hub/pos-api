package handlers

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

// terminalClaims are embedded in short-lived JWTs issued to POS terminals after PIN login.
type terminalClaims struct {
	UserID   string `json:"user_id"`
	TenantID string `json:"tenant_id"`
	OutletID string `json:"outlet_id"`
	Name     string `json:"name"`
	jwt.RegisteredClaims
}

// issueTerminalJWT signs a 4-hour HMAC-SHA256 JWT for terminal PIN sessions.
func issueTerminalJWT(member *ent.StaffMember, tenantID uuid.UUID, secret []byte) (string, error) {
	now := time.Now()
	claims := terminalClaims{
		UserID:   member.UserID.String(),
		TenantID: tenantID.String(),
		OutletID: member.OutletID.String(),
		Name:     member.Name,
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
