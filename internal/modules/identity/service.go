package identity

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
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
		// User exists — ensure StaffMember record is present (idempotent)
		s.ensureStaffMember(ctx, u.TenantID, authServiceID, claims)
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
	fullName, _ := claims["full_name"].(string)
	if fullName == "" {
		// Derive display name from email prefix if no full_name claim
		if idx := strings.Index(email, "@"); idx > 0 {
			fullName = email[:idx]
		} else {
			fullName = email
		}
	}

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

	// 5. Create StaffMember record so the user appears on the PIN login page
	s.ensureStaffMember(ctx, tenantID, authServiceID, claims)

	return newUsr, nil
}

// ensureStaffMember creates a StaffMember for the user if one does not already exist.
// Defaults to the HQ outlet if no outlet_id is present in claims.
func (s *Service) ensureStaffMember(ctx context.Context, tenantID uuid.UUID, authUserID uuid.UUID, claims map[string]any) {
	// Skip if StaffMember already exists
	exists, _ := s.client.StaffMember.Query().
		Where(entstaff.TenantID(tenantID), entstaff.UserID(authUserID)).
		Exist(ctx)
	if exists {
		return
	}

	// Resolve outlet: prefer JWT outlet_id claim, fall back to HQ outlet
	var outletID uuid.UUID
	if oid, ok := claims["outlet_id"].(string); ok && oid != "" {
		if parsed, err := uuid.Parse(oid); err == nil {
			// Verify outlet belongs to this tenant and is active
			o, err := s.client.Outlet.Query().
				Where(entoutlet.ID(parsed), entoutlet.TenantID(tenantID), entoutlet.StatusNEQ("archived")).
				Only(ctx)
			if err == nil {
				outletID = o.ID
			}
		}
	}
	if outletID == uuid.Nil {
		// Fall back to HQ outlet
		o, err := s.client.Outlet.Query().
			Where(entoutlet.TenantID(tenantID), entoutlet.IsHq(true), entoutlet.StatusNEQ("archived")).
			First(ctx)
		if err != nil {
			// Last resort: any active outlet
			o, err = s.client.Outlet.Query().
				Where(entoutlet.TenantID(tenantID), entoutlet.StatusNEQ("archived")).
				First(ctx)
		}
		if err != nil || o == nil {
			log.Printf("  [jit-provisioning] no outlet found for tenant %s — skipping StaffMember creation", tenantID)
			return
		}
		outletID = o.ID
	}

	// Map global role to POS staff role
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
	posRole := mapGlobalRoleToPOSRole(roles)
	if posRole == "viewer" {
		posRole = "cashier" // default POS role for unrecognised global roles
	}

	email, _ := claims["email"].(string)
	name, _ := claims["full_name"].(string)
	if name == "" {
		if idx := strings.Index(email, "@"); idx > 0 {
			name = email[:idx]
		} else {
			name = email
		}
	}

	created, err := s.client.StaffMember.Create().
		SetTenantID(tenantID).
		SetUserID(authUserID).
		SetName(name).
		SetRole(posRole).
		SetIsActive(true).
		Save(ctx)
	if err != nil {
		log.Printf("  [jit-provisioning] failed to create StaffMember for user %s: %v", authUserID, err)
		return
	}
	_ = s.client.StaffOutlet.Create().
		SetTenantID(tenantID).
		SetStaffMemberID(created.ID).
		SetOutletID(outletID).
		SetIsHomeOutlet(true).
		OnConflict().DoNothing().Exec(ctx)
	log.Printf("  [jit-provisioning] created StaffMember (role=%s, outlet=%s) for user %s", posRole, outletID, authUserID)
}

// assignDefaultRoleFromJWT maps global JWT roles to POS service-level roles.
// superuser/admin → admin, staff → cashier, others → viewer.
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
// Uses the canonical role names: admin (was pos_admin), manager (was store_manager).
// Backward-compatible: also accepts legacy names for existing deployments.
func mapGlobalRoleToPOSRole(roles []string) string {
	for _, r := range roles {
		switch r {
		case "superuser", "admin",
			// Legacy aliases — accepted for backward compatibility
			"pos_admin", "tenant_admin", "system_admin":
			return "admin"
		case "manager", "store_manager", "outlet_manager":
			return "manager"
		case "cashier":
			return "cashier"
		case "waiter":
			return "waiter"
		case "kitchen":
			return "kitchen"
		case "bar":
			return "bar"
		case "receptionist":
			return "receptionist"
		case "staff":
			return "cashier"
		}
	}
	return "viewer"
}
