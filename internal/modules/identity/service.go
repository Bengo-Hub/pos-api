package identity

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/user"
	"github.com/bengobox/pos-service/internal/modules/tenant"
)

// Service handles identity-related operations using Ent.
type Service struct {
	client       *ent.Client
	tenantSyncer *tenant.Syncer
}

// NewService creates a new Identity Service.
func NewService(client *ent.Client, tenantSyncer *tenant.Syncer) *Service {
	return &Service{
		client:       client,
		tenantSyncer: tenantSyncer,
	}
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
		SetID(uuid.New()).
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
	return newUsr, nil
}
