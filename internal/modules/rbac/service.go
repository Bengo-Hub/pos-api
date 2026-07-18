package rbac

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service provides business logic for RBAC operations.
type Service struct {
	repo   Repository
	logger *zap.Logger
}

// NewService creates a new RBAC service.
func NewService(repo Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger,
	}
}

// HasPermission checks if a user has a specific permission.
func (s *Service) HasPermission(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, permissionCode string) (bool, error) {
	permissions, err := s.repo.GetUserPermissions(ctx, tenantID, userID)
	if err != nil {
		return false, fmt.Errorf("get user permissions: %w", err)
	}

	for _, perm := range permissions {
		if perm.PermissionCode == permissionCode {
			return true, nil
		}
	}

	return false, nil
}

// MapGlobalRolesToServiceRole maps SSO/global JWT roles onto the canonical POS service
// role — the SAME mapping /auth/me and the terminal JWT use, exported here so the
// permission middleware resolves identically (an SSO admin whose JWT carries no pos.*
// codes and who has no per-user assignment rows still lands on the POS "admin" role).
func MapGlobalRolesToServiceRole(roles []string) string {
	order := []struct{ from, to string }{
		{"superuser", "admin"}, {"super_admin", "admin"}, {"pos_admin", "admin"},
		{"admin", "admin"},
		{"manager", "manager"}, {"store_manager", "manager"}, {"outlet_manager", "manager"},
		{"cashier", "cashier"}, {"waiter", "waiter"}, {"kitchen", "kitchen"},
		{"bar", "bar"}, {"receptionist", "receptionist"},
		{"staff", "cashier"}, {"member", "cashier"}, {"viewer", "cashier"},
	}
	for _, m := range order {
		for _, r := range roles {
			if r == m.from {
				return m.to
			}
		}
	}
	return "cashier"
}

// HasAnyPermissionViaGlobalRoles resolves permissions the way /auth/me does — from the
// POS service role(s) matching the caller's GLOBAL JWT roles — for SSO principals that
// have no POSUserRoleAssignment rows. Third leg of the middleware resolution:
// JWT canonical perms → per-user assignments → role-mapped service role.
//
// Candidates, in order: each RAW global role name tried as a POS role code first (a
// tenant-admin-created CUSTOM role assigned on the auth side carries its own code, which
// the fixed mapping below cannot know — trying only the mapped code collapsed every custom
// role to "cashier" and denied its real grants), then the fixed global→POS mapping.
func (s *Service) HasAnyPermissionViaGlobalRoles(ctx context.Context, tenantID uuid.UUID, globalRoles []string, permissionCodes ...string) (bool, error) {
	if len(permissionCodes) == 0 || len(globalRoles) == 0 {
		return false, nil
	}
	want := make(map[string]struct{}, len(permissionCodes))
	for _, code := range permissionCodes {
		want[code] = struct{}{}
	}
	candidates := make([]string, 0, len(globalRoles)+1)
	candidates = append(candidates, globalRoles...)
	candidates = append(candidates, MapGlobalRolesToServiceRole(globalRoles))
	seen := make(map[string]struct{}, len(candidates))
	for _, code := range candidates {
		if code == "" {
			continue
		}
		if _, dup := seen[code]; dup {
			continue
		}
		seen[code] = struct{}{}
		role, err := s.repo.GetRoleByCode(ctx, tenantID, code)
		if err != nil || role == nil {
			continue // not a POS role code — try the next candidate, never error the request
		}
		perms, err := s.repo.GetRolePermissions(ctx, role.ID)
		if err != nil {
			continue
		}
		for _, perm := range perms {
			if _, ok := want[perm.PermissionCode]; ok {
				return true, nil
			}
		}
	}
	return false, nil
}

// HasAnyPermission checks if a user holds at least one of the given permission codes.
// Reads the user's permissions once, then tests membership — cheaper than calling
// HasPermission per code. Returns false (not an error) when none match.
func (s *Service) HasAnyPermission(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, permissionCodes ...string) (bool, error) {
	if len(permissionCodes) == 0 {
		return false, nil
	}
	permissions, err := s.repo.GetUserPermissions(ctx, tenantID, userID)
	if err != nil {
		return false, fmt.Errorf("get user permissions: %w", err)
	}
	want := make(map[string]struct{}, len(permissionCodes))
	for _, code := range permissionCodes {
		want[code] = struct{}{}
	}
	for _, perm := range permissions {
		if _, ok := want[perm.PermissionCode]; ok {
			return true, nil
		}
	}
	return false, nil
}

// HasRole checks if a user has a specific role.
func (s *Service) HasRole(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, roleCode string) (bool, error) {
	roles, err := s.repo.GetUserRoles(ctx, tenantID, userID)
	if err != nil {
		return false, fmt.Errorf("get user roles: %w", err)
	}

	for _, role := range roles {
		if role.RoleCode == roleCode {
			return true, nil
		}
	}

	return false, nil
}

// AssignRole assigns a role to a user.
func (s *Service) AssignRole(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, roleID uuid.UUID, assignedBy uuid.UUID) error {
	// Check if assignment already exists
	assignments, err := s.repo.ListUserAssignments(ctx, tenantID, AssignmentFilters{
		UserID: &userID,
		RoleID: &roleID,
	})
	if err != nil {
		return fmt.Errorf("check existing assignment: %w", err)
	}

	if len(assignments) > 0 {
		return fmt.Errorf("role already assigned to user")
	}

	assignment := &UserRoleAssignment{
		ID:         uuid.New(),
		TenantID:   tenantID,
		UserID:     userID,
		RoleID:     roleID,
		AssignedBy: assignedBy,
	}

	if err := s.repo.AssignRoleToUser(ctx, tenantID, assignment); err != nil {
		return fmt.Errorf("assign role: %w", err)
	}

	s.logger.Info("role assigned",
		zap.String("tenant_id", tenantID.String()),
		zap.String("user_id", userID.String()),
		zap.String("role_id", roleID.String()),
		zap.String("assigned_by", assignedBy.String()),
	)

	return nil
}

// RevokeRole revokes a role from a user.
func (s *Service) RevokeRole(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, roleID uuid.UUID) error {
	if err := s.repo.RevokeRoleFromUser(ctx, tenantID, userID, roleID); err != nil {
		return fmt.Errorf("revoke role: %w", err)
	}

	s.logger.Info("role revoked",
		zap.String("tenant_id", tenantID.String()),
		zap.String("user_id", userID.String()),
		zap.String("role_id", roleID.String()),
	)

	return nil
}

// AssignRoleByCode looks up a role by code and assigns it to a user (idempotent).
func (s *Service) AssignRoleByCode(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID, assignedBy uuid.UUID, roleCode string) error {
	role, err := s.repo.GetRoleByCode(ctx, tenantID, roleCode)
	if err != nil {
		return fmt.Errorf("role %q not found: %w", roleCode, err)
	}

	// Idempotent: skip if already assigned
	assignments, err := s.repo.ListUserAssignments(ctx, tenantID, AssignmentFilters{
		UserID: &userID,
		RoleID: &role.ID,
	})
	if err == nil && len(assignments) > 0 {
		return nil
	}

	return s.AssignRole(ctx, tenantID, userID, role.ID, assignedBy)
}

// GetUserRoles retrieves all roles for a user.
func (s *Service) GetUserRoles(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID) ([]*POSRoleV2, error) {
	return s.repo.GetUserRoles(ctx, tenantID, userID)
}

// GetUserPermissions retrieves all permissions for a user.
func (s *Service) GetUserPermissions(ctx context.Context, tenantID uuid.UUID, userID uuid.UUID) ([]*POSPermission, error) {
	return s.repo.GetUserPermissions(ctx, tenantID, userID)
}
