package handlers

import (
	"encoding/json"
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/services/rbac"
	"github.com/bengobox/pos-service/internal/services/usersync"
)

// UserHandler handles user management operations
type UserHandler struct {
	logger      *zap.Logger
	rbacService *rbac.Service
	syncService *usersync.Service
}

// NewUserHandler creates a new user handler
func NewUserHandler(logger *zap.Logger, rbacService *rbac.Service, syncService *usersync.Service) *UserHandler {
	return &UserHandler{
		logger:      logger,
		rbacService: rbacService,
		syncService: syncService,
	}
}

// CreateUserRequest represents a request to create a user
type CreateUserRequest struct {
	Email      string                 `json:"email"`
	Password   string                 `json:"password,omitempty"`
	TenantSlug string                 `json:"tenant_slug"`
	Profile    map[string]interface{} `json:"profile,omitempty"`
	Roles      []string               `json:"roles,omitempty"`
}

// CreateUser creates a new user and syncs with auth-service
func (h *UserHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	// Extract tenant ID from context (set by auth middleware)
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok {
		respondJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	tenantID, _ := claims.TenantUUID()
	if tenantID == nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant ID required"})
		return
	}

	// Sync user with auth-service
	syncReq := usersync.SyncUserRequest{
		Email:      req.Email,
		Password:   req.Password,
		TenantSlug: req.TenantSlug,
		Profile:    req.Profile,
		Service:    "pos-service",
	}

	syncResp, err := h.syncService.SyncUser(r.Context(), syncReq)
	if err != nil {
		h.logger.Error("failed to sync user with auth-service", zap.Error(err))
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to sync user"})
		return
	}

	// In a real implementation, you would:
	// 1. Create user in local database
	// 2. Assign roles
	// 3. Return user details

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"user_id":   syncResp.UserID,
		"email":     syncResp.Email,
		"tenant_id": syncResp.TenantID,
		"created":   true,
	})
}

// GetUserRoles returns the roles for the current user
func (h *UserHandler) GetUserRoles(w http.ResponseWriter, r *http.Request) {
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok {
		respondJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	userID, err := claims.UserID()
	if err != nil || userID == uuid.Nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "user ID required"})
		return
	}

	tenantID, err := claims.TenantUUID()
	if err != nil || tenantID == nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant ID required"})
		return
	}

	roles, err := h.rbacService.GetUserRoles(r.Context(), userID, *tenantID)
	if err != nil {
		h.logger.Error("failed to get user roles", zap.Error(err))
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to get roles"})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"roles": roles,
	})
}

// ListRoles returns all available roles
func (h *UserHandler) ListRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := h.rbacService.ListRoles(r.Context())
	if err != nil {
		h.logger.Error("failed to list roles", zap.Error(err))
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list roles"})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"roles": roles,
	})
}

// RegisterRoutes registers user management routes
func (h *UserHandler) RegisterRoutes(r chi.Router) {
	r.Post("/users", h.CreateUser)
	r.Get("/users/me/roles", h.GetUserRoles)
	r.Get("/roles", h.ListRoles)
}
