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

// adminOverrideRoles may create/edit roles and manage role permissions.
var adminOverrideRoles = map[string]bool{
	"admin": true, "manager": true, "pos_admin": true, "store_manager": true,
	"owner": true, "super_admin": true, "superuser": true,
}

// canManageRBAC reports whether the caller may manage roles/permissions.
// Reuses requesterRole (pos_role context) and falls back to JWT role claims /
// platform-owner so SSO admins (no pos_role) are also permitted.
func (h *RBACHandler) canManageRBAC(w http.ResponseWriter, r *http.Request) bool {
	if adminOverrideRoles[requesterRole(r)] {
		return true
	}
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims != nil {
		if claims.IsPlatformOwner {
			return true
		}
		for _, role := range claims.Roles {
			if adminOverrideRoles[role] {
				return true
			}
		}
	}
	respondJSON(w, http.StatusForbidden, map[string]string{"error": "insufficient permissions"})
	return false
}

type createPOSRoleRequest struct {
	RoleCode    string `json:"role_code"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// CreateRole handles POST /rbac/roles — creates a tenant-scoped custom POS role.
func (h *RBACHandler) CreateRole(w http.ResponseWriter, r *http.Request) {
	if !h.canManageRBAC(w, r) {
		return
	}
	tid, err := uuid.Parse(httpware.GetTenantID(r.Context()))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid tenant_id"})
		return
	}
	var req createPOSRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RoleCode == "" || req.Name == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "role_code and name are required"})
		return
	}
	role := &rbac.POSRoleV2{
		ID:           uuid.New(),
		TenantID:     &tid,
		RoleCode:     req.RoleCode,
		Name:         req.Name,
		IsSystemRole: false,
	}
	if req.Description != "" {
		role.Description = &req.Description
	}
	if err := h.rbacRepo.CreateRole(r.Context(), tid, role); err != nil {
		h.logger.Error("create role failed", zap.Error(err))
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create role"})
		return
	}
	respondJSON(w, http.StatusCreated, role)
}

// GetRolePermissions handles GET /rbac/roles/{roleID}/permissions.
func (h *RBACHandler) GetRolePermissions(w http.ResponseWriter, r *http.Request) {
	roleID, err := uuid.Parse(chi.URLParam(r, "roleID"))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid role id"})
		return
	}
	perms, err := h.rbacRepo.GetRolePermissions(r.Context(), roleID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load role permissions"})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"data": perms, "permissions": perms})
}

type setPOSRolePermissionsRequest struct {
	PermissionIDs []uuid.UUID `json:"permission_ids"`
}

// SetRolePermissions handles PUT /rbac/roles/{roleID}/permissions — replaces a
// role's permission set (the permission-matrix save).
func (h *RBACHandler) SetRolePermissions(w http.ResponseWriter, r *http.Request) {
	if !h.canManageRBAC(w, r) {
		return
	}
	roleID, err := uuid.Parse(chi.URLParam(r, "roleID"))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid role id"})
		return
	}
	var req setPOSRolePermissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	ctx := r.Context()
	current, err := h.rbacRepo.GetRolePermissions(ctx, roleID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load role permissions"})
		return
	}
	have := make(map[uuid.UUID]bool, len(current))
	for _, p := range current {
		have[p.ID] = true
	}
	want := make(map[uuid.UUID]bool, len(req.PermissionIDs))
	for _, id := range req.PermissionIDs {
		want[id] = true
	}
	for id := range want {
		if !have[id] {
			if err := h.rbacRepo.AssignPermissionToRole(ctx, roleID, id); err != nil {
				h.logger.Warn("assign permission failed", zap.String("permission", id.String()), zap.Error(err))
			}
		}
	}
	for id := range have {
		if !want[id] {
			if err := h.rbacRepo.RemovePermissionFromRole(ctx, roleID, id); err != nil {
				h.logger.Warn("remove permission failed", zap.String("permission", id.String()), zap.Error(err))
			}
		}
	}
	respondJSON(w, http.StatusOK, map[string]string{"message": "role permissions updated"})
}
