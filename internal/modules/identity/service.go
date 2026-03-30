package identity

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/user"
	"github.com/bengobox/pos-service/internal/modules/rbac"
	"github.com/bengobox/pos-service/internal/modules/tenant"
)

// Service handles identity-related operations using Ent.
type Service struct {
	client       *ent.Client
	tenantSyncer *tenant.Syncer
	rbacService  *rbac.Service
}

// NewService creates a new Identity Service.
func NewService(client *ent.Client, tenantSyncer *tenant.Syncer) *Service {
	return &Service{
		client:       client,
		tenantSyncer: tenantSyncer,
	}
}

// SetRBACService sets the RBAC service for JIT role assignment.
func (s *Service) SetRBACService(svc *rbac.Service) {
	s.rbacService = svc
}

// EnsureUserFromToken performs JIT (Just-In-Time) provisioning of users and tenants.
// If the user doesn't exist locally, it creates them. If the tenant doesn't exist,
// it syncs it from the auth-service first.
// Platform admin (tenant_slug=codevertex + superuser in JWT) has full access via IsPlatformOwner in router; no local role assignment needed.
func (s *Service) EnsureUserFromToken(ctx context.Context, authServiceID uuid.UUID, tenantSlug string, claims map[string]any) (*ent.User, error) {
	// 1. Check if user exists by auth_service_id
	u, err := s.client.User.Query().
		Where(user.AuthServiceUserIDEQ(authServiceID)).
		Only(ctx)
	
	if err == nil {
		return u, nil
	}

	if !ent.IsNotFound(err) {
		return nil, fmt.Errorf("identity.Service: query user: %w", err)
	}

	// 2. User not found, ensure tenant exists
	tenantID, err := s.tenantSyncer.SyncTenant(ctx, tenantSlug)
	if err != nil {
		return nil, fmt.Errorf("identity.Service: sync tenant %q: %w", tenantSlug, err)
	}

	// 3. Create user
	email, _ := claims["email"].(string)
	fullName, _ := claims["name"].(string)
	
	newUsr, err := s.client.User.Create().
		SetID(authServiceID).
		SetAuthServiceUserID(authServiceID).
		SetTenantID(tenantID).
		SetEmail(email).
		SetFullName(fullName).
		SetStatus("active").
		SetSyncStatus("synced").
		SetSyncAt(time.Now()).
		Save(ctx)

	if err != nil {
		return nil, fmt.Errorf("identity.Service: create user: %w", err)
	}

	log.Printf("  [jit-provisioning] created user %s (AuthID %s) for tenant %s", email, authServiceID, tenantSlug)

	// 4. Assign default POS role based on JWT roles
	if s.rbacService != nil {
		s.assignDefaultRoleFromJWT(ctx, tenantID, newUsr.ID, authServiceID, claims)
	}

	return newUsr, nil
}

// assignDefaultRoleFromJWT maps global JWT roles to POS service-level roles.
// superuser/admin → pos_admin, staff → cashier, others → viewer.
func (s *Service) assignDefaultRoleFromJWT(ctx context.Context, tenantID uuid.UUID, localUserID uuid.UUID, authUserID uuid.UUID, claims map[string]any) {
	var roles []string
	if rolesRaw, ok := claims["roles"].([]string); ok {
		roles = rolesRaw
	} else if rolesIface, ok := claims["roles"].([]interface{}); ok {
		for _, r := range rolesIface {
			if str, ok := r.(string); ok {
				roles = append(roles, str)
			}
		}
	}

	roleCode := mapGlobalRoleToPOSRole(roles)
	if roleCode == "" {
		return
	}

	if err := s.rbacService.AssignRoleByCode(ctx, tenantID, localUserID, authUserID, roleCode); err != nil {
		log.Printf("  [jit-provisioning] role assignment failed for %s: %v", roleCode, err)
	} else {
		log.Printf("  [jit-provisioning] assigned POS role %s to user %s", roleCode, localUserID)
	}
}

// mapGlobalRoleToPOSRole maps global SSO roles to POS service roles.
func mapGlobalRoleToPOSRole(roles []string) string {
	for _, r := range roles {
		switch r {
		case "superuser", "admin":
			return "pos_admin"
		case "staff":
			return "cashier"
		}
	}
	return "viewer"
}
