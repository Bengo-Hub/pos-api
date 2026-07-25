package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/serviceconfig"
)

// ConfigKeyScreensaverIdleTimeoutSeconds is the service_config key holding the
// POS terminal screensaver idle-timeout (in seconds). A row with tenant_id IS NULL
// is the platform default; a row with tenant_id set is a tenant override.
const ConfigKeyScreensaverIdleTimeoutSeconds = "pos.screensaver_idle_timeout_seconds"

// resolveScreensaverTimeoutSeconds resolves the screensaver idle-timeout for a
// tenant. It prefers the tenant override (tenant_id == tenantID) over the
// platform default (tenant_id IS NULL). It returns 0 when no valid config exists
// (callers should then apply their own app default).
func resolveScreensaverTimeoutSeconds(ctx context.Context, client *ent.Client, tenantID uuid.UUID) int {
	rows, err := client.ServiceConfig.Query().
		Where(
			serviceconfig.ConfigKey(ConfigKeyScreensaverIdleTimeoutSeconds),
			serviceconfig.Or(
				serviceconfig.TenantID(tenantID),
				serviceconfig.TenantIDIsNil(),
			),
		).
		All(ctx)
	if err != nil || len(rows) == 0 {
		return 0
	}

	var platformValue string
	var tenantValue string
	for _, row := range rows {
		if row.TenantID != nil && *row.TenantID == tenantID {
			tenantValue = row.ConfigValue
		} else if row.TenantID == nil {
			platformValue = row.ConfigValue
		}
	}

	chosen := platformValue
	if tenantValue != "" {
		chosen = tenantValue
	}

	v, err := strconv.Atoi(strings.TrimSpace(chosen))
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// ServiceConfigHandler handles platform-level service configuration CRUD.
type ServiceConfigHandler struct {
	client *ent.Client
	logger *zap.Logger
}

// NewServiceConfigHandler creates a new ServiceConfigHandler.
func NewServiceConfigHandler(client *ent.Client, logger *zap.Logger) *ServiceConfigHandler {
	return &ServiceConfigHandler{client: client, logger: logger}
}

type serviceConfigResponse struct {
	ID          uuid.UUID  `json:"id"`
	TenantID    *uuid.UUID `json:"tenant_id,omitempty"`
	ConfigKey   string     `json:"config_key"`
	ConfigValue string     `json:"config_value"`
	ConfigType  string     `json:"config_type"`
	Description string     `json:"description,omitempty"`
	IsSecret    bool       `json:"is_secret"`
	IsOverride  bool       `json:"is_override"`
	CreatedAt   string     `json:"created_at"`
	UpdatedAt   string     `json:"updated_at"`
}

func toSCResponse(cfg *ent.ServiceConfig, isOverride bool) serviceConfigResponse {
	val := cfg.ConfigValue
	if cfg.IsSecret {
		val = "***"
	}
	return serviceConfigResponse{
		ID:          cfg.ID,
		TenantID:    cfg.TenantID,
		ConfigKey:   cfg.ConfigKey,
		ConfigValue: val,
		ConfigType:  cfg.ConfigType,
		Description: cfg.Description,
		IsSecret:    cfg.IsSecret,
		IsOverride:  isOverride,
		CreatedAt:   cfg.CreatedAt.Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   cfg.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}
}

// ListPlatformConfigs returns all platform-level (tenant_id=nil) service configs.
// GET /api/v1/admin/config
func (h *ServiceConfigHandler) ListPlatformConfigs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	configs, err := h.client.ServiceConfig.Query().
		Where(serviceconfig.TenantIDIsNil()).
		Order(ent.Asc(serviceconfig.FieldConfigKey)).
		All(ctx)
	if err != nil {
		h.logger.Error("failed to list platform service configs", zap.Error(err))
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	result := make([]serviceConfigResponse, 0, len(configs))
	for _, c := range configs {
		result = append(result, toSCResponse(c, false))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": result, "total": len(result)})
}

// UpsertPlatformConfig creates or updates a platform-level service config entry by key.
// PUT /api/v1/admin/config/{key}
func (h *ServiceConfigHandler) UpsertPlatformConfig(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "" {
		http.Error(w, `{"error":"config key is required"}`, http.StatusBadRequest)
		return
	}

	var body struct {
		ConfigValue string `json:"config_value"`
		ConfigType  string `json:"config_type,omitempty"`
		Description string `json:"description,omitempty"`
		IsSecret    bool   `json:"is_secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if body.ConfigValue == "" {
		http.Error(w, `{"error":"config_value is required"}`, http.StatusBadRequest)
		return
	}
	if body.ConfigType == "" {
		body.ConfigType = "string"
	}

	ctx := r.Context()

	existing, _ := h.client.ServiceConfig.Query().
		Where(serviceconfig.ConfigKey(key), serviceconfig.TenantIDIsNil()).
		First(ctx)

	var cfg *ent.ServiceConfig
	var err error

	if existing != nil {
		upd := existing.Update().
			SetConfigValue(body.ConfigValue).
			SetIsSecret(body.IsSecret)
		if body.Description != "" {
			upd = upd.SetDescription(body.Description)
		}
		if body.ConfigType != "" {
			upd = upd.SetConfigType(body.ConfigType)
		}
		cfg, err = upd.Save(ctx)
	} else {
		id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("sc::pos::"+key))
		create := h.client.ServiceConfig.Create().
			SetID(id).
			SetConfigKey(key).
			SetConfigValue(body.ConfigValue).
			SetConfigType(body.ConfigType).
			SetIsSecret(body.IsSecret)
		if body.Description != "" {
			create = create.SetDescription(body.Description)
		}
		cfg, err = create.Save(ctx)
	}
	if err != nil {
		h.logger.Error("failed to upsert platform service config", zap.Error(err), zap.String("key", key))
		http.Error(w, `{"error":"failed to save config"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toSCResponse(cfg, false))
}

// ListTenantOverrides returns a specific tenant's config overrides (tenant_id=tenantID).
// GET /api/v1/admin/tenants/{tenantID}/config
func (h *ServiceConfigHandler) ListTenantOverrides(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(chi.URLParam(r, "tenantID"))
	if err != nil {
		http.Error(w, `{"error":"invalid tenant_id"}`, http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	configs, err := h.client.ServiceConfig.Query().
		Where(serviceconfig.TenantID(tenantID)).
		Order(ent.Asc(serviceconfig.FieldConfigKey)).
		All(ctx)
	if err != nil {
		h.logger.Error("failed to list tenant service configs", zap.Error(err))
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	result := make([]serviceConfigResponse, 0, len(configs))
	for _, c := range configs {
		result = append(result, toSCResponse(c, true))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": result, "total": len(result)})
}

// UpsertTenantOverride creates or updates a config override for a SPECIFIC tenant, chosen by the
// platform owner (distinct from any tenant self-service settings route — this lets platform staff
// grant one tenant an exception, e.g. the provider-footer opt-out, without that tenant being able
// to set it themselves).
// PUT /api/v1/admin/tenants/{tenantID}/config/{key}
func (h *ServiceConfigHandler) UpsertTenantOverride(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(chi.URLParam(r, "tenantID"))
	if err != nil {
		http.Error(w, `{"error":"invalid tenant_id"}`, http.StatusBadRequest)
		return
	}
	key := chi.URLParam(r, "key")
	if key == "" {
		http.Error(w, `{"error":"config key is required"}`, http.StatusBadRequest)
		return
	}

	var body struct {
		ConfigValue string `json:"config_value"`
		Description string `json:"description,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if body.ConfigValue == "" {
		http.Error(w, `{"error":"config_value is required"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Inherit config_type/is_secret from the platform default when one exists, so a tenant
	// override never diverges in type from the key's platform-wide definition.
	platformDefault, _ := h.client.ServiceConfig.Query().
		Where(serviceconfig.ConfigKey(key), serviceconfig.TenantIDIsNil()).
		First(ctx)
	configType := "string"
	isSecret := false
	if platformDefault != nil {
		configType = platformDefault.ConfigType
		isSecret = platformDefault.IsSecret
	}

	existing, _ := h.client.ServiceConfig.Query().
		Where(serviceconfig.ConfigKey(key), serviceconfig.TenantID(tenantID)).
		First(ctx)

	var cfg *ent.ServiceConfig
	if existing != nil {
		upd := existing.Update().SetConfigValue(body.ConfigValue)
		if body.Description != "" {
			upd = upd.SetDescription(body.Description)
		}
		cfg, err = upd.Save(ctx)
	} else {
		create := h.client.ServiceConfig.Create().
			SetTenantID(tenantID).
			SetConfigKey(key).
			SetConfigValue(body.ConfigValue).
			SetConfigType(configType).
			SetIsSecret(isSecret)
		if body.Description != "" {
			create = create.SetDescription(body.Description)
		} else if platformDefault != nil {
			create = create.SetDescription(platformDefault.Description)
		}
		cfg, err = create.Save(ctx)
	}
	if err != nil {
		h.logger.Error("failed to upsert tenant override", zap.Error(err), zap.String("key", key))
		http.Error(w, `{"error":"failed to save config"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toSCResponse(cfg, true))
}

// DeleteTenantOverride removes a tenant's override for a key, reverting it to inherit the
// platform default.
// DELETE /api/v1/admin/tenants/{tenantID}/config/{key}
func (h *ServiceConfigHandler) DeleteTenantOverride(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(chi.URLParam(r, "tenantID"))
	if err != nil {
		http.Error(w, `{"error":"invalid tenant_id"}`, http.StatusBadRequest)
		return
	}
	key := chi.URLParam(r, "key")
	if key == "" {
		http.Error(w, `{"error":"config key is required"}`, http.StatusBadRequest)
		return
	}
	if _, err := h.client.ServiceConfig.Delete().
		Where(serviceconfig.ConfigKey(key), serviceconfig.TenantID(tenantID)).
		Exec(r.Context()); err != nil {
		h.logger.Error("failed to delete tenant override", zap.Error(err), zap.String("key", key))
		http.Error(w, `{"error":"failed to delete override"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RegisterAdminRoutes registers platform admin service config routes.
// Caller is responsible for applying platform-owner auth middleware.
func (h *ServiceConfigHandler) RegisterAdminRoutes(r chi.Router) {
	r.Get("/admin/config", h.ListPlatformConfigs)
	r.Put("/admin/config/{key}", h.UpsertPlatformConfig)
	r.Get("/admin/tenants/{tenantID}/config", h.ListTenantOverrides)
	r.Put("/admin/tenants/{tenantID}/config/{key}", h.UpsertTenantOverride)
	r.Delete("/admin/tenants/{tenantID}/config/{key}", h.DeleteTenantOverride)
}
