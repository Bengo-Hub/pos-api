package usersync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// Service handles user synchronization with auth-service SSO
type Service struct {
	authServiceURL string
	apiKey         string
	httpClient     *http.Client
	logger         *zap.Logger
}

// NewService creates a new user sync service
func NewService(authServiceURL, apiKey string, logger *zap.Logger) *Service {
	return &Service{
		authServiceURL: authServiceURL,
		apiKey:         apiKey,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		logger: logger,
	}
}

// SyncUserRequest represents the request to sync a user with auth-service
type SyncUserRequest struct {
	Email      string                 `json:"email"`
	Password   string                 `json:"password,omitempty"`
	TenantSlug string                 `json:"tenant_slug"`
	Profile    map[string]interface{} `json:"profile,omitempty"`
	Service    string                 `json:"service,omitempty"`
}

// SyncUserResponse represents the response from auth-service
type SyncUserResponse struct {
	UserID   string `json:"user_id"`
	Email    string `json:"email"`
	TenantID string `json:"tenant_id"`
	Created  bool   `json:"created"`
	Message  string `json:"message"`
}

// SyncUser syncs a user with auth-service SSO
func (s *Service) SyncUser(ctx context.Context, req SyncUserRequest) (*SyncUserResponse, error) {
	if s.apiKey == "" {
		s.logger.Warn("auth-service API key not configured, skipping user sync")
		return nil, fmt.Errorf("auth-service API key not configured")
	}

	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal sync request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", s.authServiceURL+"/api/v1/admin/users/sync", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create sync request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", s.apiKey)

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sync user request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var errResp map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errResp)
		s.logger.Warn("user sync failed",
			zap.Int("status", resp.StatusCode),
			zap.Any("error", errResp),
			zap.String("email", req.Email),
		)
		return nil, fmt.Errorf("user sync failed: status %d", resp.StatusCode)
	}

	var syncResp SyncUserResponse
	if err := json.NewDecoder(resp.Body).Decode(&syncResp); err != nil {
		return nil, fmt.Errorf("decode sync response: %w", err)
	}

	s.logger.Info("user synced with auth-service",
		zap.String("user_id", syncResp.UserID),
		zap.String("email", syncResp.Email),
		zap.Bool("created", syncResp.Created),
	)

	return &syncResp, nil
}

