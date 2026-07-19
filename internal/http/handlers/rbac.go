package handlers

import (
	"encoding/json"
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/Bengo-Hub/httpware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/rbac"
)

// RBACHandler handles RBAC-related operations.
type RBACHandler struct {
	logger      *zap.Logger
	rbacService *rbac.Service
	rbacRepo    rbac.Repository
	// authURL + authAPIKey enable best-effort push of tenant CUSTOM roles to auth's Role registry
	// on create, so admins can also assign them via auth-ui (SSO). Empty = skip (pos still resolves
	// custom roles locally by role_code).
	authURL    string
	authAPIKey string
}

// NewRBACHandler creates a new RBAC handler.
func NewRBACHandler(logger *zap.Logger, rbacService *rbac.Service, rbacRepo rbac.Repository, authURL, authAPIKey string) *RBACHandler {
	return &RBACHandler{
		logger:      logger,
		rbacService: rbacService,
		rbacRepo:    rbacRepo,
		authURL:     authURL,
		authAPIKey:  authAPIKey,
	}
}

// AssignRoleRequest represents a request to assign a role.
type AssignRoleRequest struct {
	UserID uuid.UUID `json:"user_id"`
	RoleID uuid.UUID `json:"role_id"`
}

// AssignRole assigns a role to a user.
func (h *RBACHandler) AssignRole(w http.ResponseWriter, r *http.Request) {
	if !h.canManageRBAC(w, r) {
		return
	}
	tenantID := httpware.GetTenantID(r.Context())
	if tenantID == "" {
		jsonError(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var req AssignRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Guardrail: a manager may not assign a privileged (admin/manager) role — only an
	// admin may. Resolve the target role's code so the check works for system roles too.
	if role, gerr := h.rbacRepo.GetRole(r.Context(), tid, req.RoleID); gerr == nil {
		if managementProtectedRoles[canonicalizeRole(role.RoleCode)] && !requesterIsAdminLevel(r) {
			jsonError(w, "managers cannot assign admin or manager roles", http.StatusForbidden)
			return
		}
	}

	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	assignedBy, err := claims.UserID()
	if err != nil || assignedBy == uuid.Nil {
		jsonError(w, "invalid user ID", http.StatusUnauthorized)
		return
	}

	if err := h.rbacService.AssignRole(r.Context(), tid, req.UserID, req.RoleID, assignedBy); err != nil {
		h.logger.Error("failed to assign role", zap.Error(err))
		jsonError(w, "failed to assign role", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusCreated, map[string]string{"message": "role assigned successfully"})
}

// RevokeRole revokes a role from a user.
func (h *RBACHandler) RevokeRole(w http.ResponseWriter, r *http.Request) {
	if !h.canManageRBAC(w, r) {
		return
	}
	tenantID := httpware.GetTenantID(r.Context())
	if tenantID == "" {
		jsonError(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	assignmentIDStr := chi.URLParam(r, "id")
	assignmentID, err := uuid.Parse(assignmentIDStr)
	if err != nil {
		jsonError(w, "invalid assignment ID", http.StatusBadRequest)
		return
	}

	// Get assignment to extract user ID and role ID
	assignments, err := h.rbacRepo.ListUserAssignments(r.Context(), tid, rbac.AssignmentFilters{})
	if err != nil {
		jsonError(w, "failed to get assignment", http.StatusInternalServerError)
		return
	}

	var assignment *rbac.UserRoleAssignment
	for _, a := range assignments {
		if a.ID == assignmentID {
			assignment = a
			break
		}
	}

	if assignment == nil {
		jsonError(w, "assignment not found", http.StatusNotFound)
		return
	}

	if err := h.rbacService.RevokeRole(r.Context(), tid, assignment.UserID, assignment.RoleID); err != nil {
		h.logger.Error("failed to revoke role", zap.Error(err))
		jsonError(w, "failed to revoke role", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"message": "role revoked successfully"})
}

// ListAssignments lists all role assignments.
func (h *RBACHandler) ListAssignments(w http.ResponseWriter, r *http.Request) {
	tenantID := httpware.GetTenantID(r.Context())
	if tenantID == "" {
		jsonError(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	assignments, err := h.rbacRepo.ListUserAssignments(r.Context(), tid, rbac.AssignmentFilters{})
	if err != nil {
		h.logger.Error("failed to list assignments", zap.Error(err))
		jsonError(w, "failed to list assignments", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"assignments": assignments})
}

// roleWithScope wraps a role with its resolved use_cases so the frontend can label/badge it
// (e.g. "pharmacy only") even when the caller didn't filter by use_case.
type roleWithScope struct {
	*rbac.POSRoleV2
	UseCases []string `json:"use_cases,omitempty"`
}

// ListRoles lists roles visible to the tenant, optionally filtered to a single outlet use case
// (?use_case=hospitality|retail|services|quick_service|pharmacy). System roles are scoped via
// a static map (rbac.systemRoleUseCases); custom roles are scoped dynamically from the modules
// of their currently granted permissions — so a hospitality outlet's Team/Roles UI doesn't show
// pharmacy/services/retail-exclusive roles, and vice versa. A role with only common-module
// grants (or none yet) always matches every use case.
func (h *RBACHandler) ListRoles(w http.ResponseWriter, r *http.Request) {
	tenantID := httpware.GetTenantID(r.Context())
	if tenantID == "" {
		jsonError(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	useCase := r.URL.Query().Get("use_case")

	roles, err := h.rbacRepo.ListRoles(r.Context(), tid)
	if err != nil {
		h.logger.Error("failed to list roles", zap.Error(err))
		jsonError(w, "failed to list roles", http.StatusInternalServerError)
		return
	}

	out := make([]roleWithScope, 0, len(roles))
	for _, role := range roles {
		var modules []string
		if !role.IsSystemRole {
			perms, perr := h.rbacRepo.GetRolePermissions(r.Context(), role.ID)
			if perr == nil {
				seen := map[string]bool{}
				for _, p := range perms {
					if !seen[p.Module] {
						seen[p.Module] = true
						modules = append(modules, p.Module)
					}
				}
			}
		}
		scopes := rbac.RoleUseCases(role.RoleCode, role.IsSystemRole, modules)
		if !rbac.RoleMatchesUseCase(scopes, useCase) {
			continue
		}
		out = append(out, roleWithScope{POSRoleV2: role, UseCases: scopes})
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"roles": out})
}

// ListPermissions lists permissions, optionally filtered to a single outlet use case
// (?use_case=hospitality|retail|services|quick_service|pharmacy). A module with no use-case
// restriction (see rbac.moduleUseCases) is common and always included.
func (h *RBACHandler) ListPermissions(w http.ResponseWriter, r *http.Request) {
	module := r.URL.Query().Get("module")
	action := r.URL.Query().Get("action")
	useCase := r.URL.Query().Get("use_case")

	filters := rbac.PermissionFilters{}
	if module != "" {
		filters.Module = &module
	}
	if action != "" {
		filters.Action = &action
	}

	permissions, err := h.rbacRepo.ListPermissions(r.Context(), filters)
	if useCase != "" && err == nil {
		filtered := make([]*rbac.POSPermission, 0, len(permissions))
		for _, p := range permissions {
			if rbac.ModuleMatchesUseCase(p.Module, useCase) {
				filtered = append(filtered, p)
			}
		}
		permissions = filtered
	}
	if err != nil {
		h.logger.Error("failed to list permissions", zap.Error(err))
		jsonError(w, "failed to list permissions", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"permissions": permissions})
}

// GetUserPermissions returns all permissions for the calling user.
func (h *RBACHandler) GetUserPermissions(w http.ResponseWriter, r *http.Request) {
	tenantID := httpware.GetTenantID(r.Context())
	if tenantID == "" {
		jsonError(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	userIDStr := chi.URLParam(r, "userID")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		jsonError(w, "invalid user_id", http.StatusBadRequest)
		return
	}

	permissions, err := h.rbacService.GetUserPermissions(r.Context(), tid, userID)
	if err != nil {
		h.logger.Error("failed to get user permissions", zap.Error(err))
		jsonError(w, "failed to get user permissions", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"permissions": permissions})
}

// GetUserRoles returns all roles for a specific user.
func (h *RBACHandler) GetUserRoles(w http.ResponseWriter, r *http.Request) {
	tenantID := httpware.GetTenantID(r.Context())
	if tenantID == "" {
		jsonError(w, "tenant_id required", http.StatusBadRequest)
		return
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	userIDStr := chi.URLParam(r, "userID")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		jsonError(w, "invalid user_id", http.StatusBadRequest)
		return
	}

	roles, err := h.rbacService.GetUserRoles(r.Context(), tid, userID)
	if err != nil {
		h.logger.Error("failed to get user roles", zap.Error(err))
		jsonError(w, "failed to get user roles", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"roles": roles})
}

// RegisterRoutes registers RBAC routes on the provided chi.Router.
func (h *RBACHandler) RegisterRoutes(r chi.Router) {
	r.Post("/rbac/assignments", h.AssignRole)
	r.Get("/rbac/assignments", h.ListAssignments)
	r.Delete("/rbac/assignments/{id}", h.RevokeRole)
	r.Get("/rbac/roles", h.ListRoles)
	r.Post("/rbac/roles", h.CreateRole)
	r.Patch("/rbac/roles/{roleID}", h.UpdateRole)
	r.Delete("/rbac/roles/{roleID}", h.DeleteRole)
	r.Get("/rbac/roles/{roleID}/permissions", h.GetRolePermissions)
	r.Put("/rbac/roles/{roleID}/permissions", h.SetRolePermissions)
	r.Get("/rbac/permissions", h.ListPermissions)
	r.Get("/rbac/users/{userID}/permissions", h.GetUserPermissions)
	r.Get("/rbac/users/{userID}/roles", h.GetUserRoles)
}
