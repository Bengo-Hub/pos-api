package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
)

// permissionChecker is the subset of the rbac.Service used for the DB fallback.
// Declared as an interface so this package doesn't import the rbac module (avoids
// an import cycle and keeps the middleware decoupled).
type permissionChecker interface {
	HasAnyPermission(ctx context.Context, tenantID, userID uuid.UUID, permissionCodes ...string) (bool, error)
}

// RequireServicePermission returns middleware that allows the request only when the
// authenticated principal satisfies at least one of the given permission codes.
//
// Resolution order (mirrors treasury-api's requireServicePermission):
//  1. no/empty claims                       -> 401 unauthorized
//  2. superuser or platform owner           -> allow (bypass)
//  3. JWT canonical permissions (claims)    -> allow if HasAnyPermission
//  4. local RBAC DB (tenant-scoped roles)   -> allow if HasAnyPermission
//  5. otherwise                             -> 403 permission_denied
//
// The local-RBAC fallback (4) is what lets tenant admins/managers through when their
// JWT was minted before a permission existed but the role carries it in the DB.
func RequireServicePermission(rbac permissionChecker, permissions ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := authclient.ClaimsFromContext(r.Context())
			if !ok || claims == nil || claims.Subject == "" {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}

			// Superuser / platform owner bypass.
			if claims.IsSuperuser() || claims.IsPlatformOwner {
				next.ServeHTTP(w, r)
				return
			}

			// JWT-level canonical permissions.
			if claims.HasAnyPermission(permissions...) {
				next.ServeHTTP(w, r)
				return
			}

			// Local RBAC DB fallback (tenant-scoped).
			if rbac != nil {
				tenantID, terr := uuid.Parse(claims.TenantID)
				userID, uerr := uuid.Parse(claims.Subject)
				if terr == nil && uerr == nil && tenantID != uuid.Nil && userID != uuid.Nil {
					if has, err := rbac.HasAnyPermission(r.Context(), tenantID, userID, permissions...); err == nil && has {
						next.ServeHTTP(w, r)
						return
					}
				}
			}

			writeJSON(w, http.StatusForbidden, map[string]any{
				"error":    "permission_denied",
				"message":  "you do not have permission to perform this action",
				"required": strings.Join(permissions, " | "),
			})
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
