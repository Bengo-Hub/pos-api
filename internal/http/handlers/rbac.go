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
}

// NewRBACHandler creates a new RBAC handler.
func NewRBACHandler(logger *zap.Logger, rbacService *rbac.Service, rbacRepo rbac.Repository) *RBACHandler {
	return &RBACHandler{
		logger:      logger,
		rbacService: rbacService,
		rbacRepo:    rbacRepo,
	}
}

// AssignRoleRequest represents a request to assign a role.
type AssignRoleRequest struct {
	UserID uuid.UUID `json:"user_id"`
	RoleID uuid.UUID `json:"role_id"`
}

// AssignRole assigns a role to a user.
func (h *RBACHandler) AssignRole(w http.ResponseWriter, r *http.Request) {
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

// ListRoles lists all roles.
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

	roles, err := h.rbacRepo.ListRoles(r.Context(), tid)
	if err != nil {
		h.logger.Error("failed to list roles", zap.Error(err))
		jsonError(w, "failed to list roles", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"roles": roles})
}

// ListPermissions lists all permissions.
func (h *RBACHandler) ListPermissions(w http.ResponseWriter, r *http.Request) {
	module := r.URL.Query().Get("module")
	action := r.URL.Query().Get("action")

	filters := rbac.PermissionFilters{}
	if module != "" {
		filters.Module = &module
	}
	if action != "" {
		filters.Action = &action
	}

	permissions, err := h.rbacRepo.ListPermissions(r.Context(), filters)
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
	r.Get("/rbac/permissions", h.ListPermissions)
	r.Get("/rbac/users/{userID}/permissions", h.GetUserPermissions)
	r.Get("/rbac/users/{userID}/roles", h.GetUserRoles)
}
