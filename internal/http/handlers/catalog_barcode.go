package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
)

const barcodeCacheTTL = 5 * time.Minute

// SetRedisClient wires a Redis client into CatalogHandler for barcode caching.
func (h *CatalogHandler) SetRedisClient(rc *redis.Client) {
	h.redis = rc
}

// BarcodeLookup handles GET /{tenantID}/pos/catalog/barcode/{barcode}
// Proxies to inventory-api for barcode lookup; merges with local POSCatalogOverride for pricing.
func (h *CatalogHandler) BarcodeLookup(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	barcode := chi.URLParam(r, "barcode")
	if barcode == "" {
		jsonError(w, "barcode is required", http.StatusBadRequest)
		return
	}

	tenantSlug := ""
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		tenantSlug = claims.GetTenantSlug()
	}
	if tenantSlug == "" {
		tenantSlug = httpware.GetTenantSlug(r.Context())
	}
	if tenantSlug == "" {
		if t, lookupErr := h.client.Tenant.Get(r.Context(), tid); lookupErr == nil {
			tenantSlug = t.Slug
		}
	}
	if tenantSlug == "" {
		jsonError(w, "tenant slug required", http.StatusBadRequest)
		return
	}

	cacheKey := fmt.Sprintf("pos:barcode:%s:%s", tid, barcode)

	// Try Redis cache first
	if h.redis != nil {
		if cached, cacheErr := h.redis.Get(r.Context(), cacheKey).Bytes(); cacheErr == nil {
			h.log.Debug("barcode cache hit", zap.String("barcode", barcode))
			w.Header().Set("X-Cache", "HIT")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cached)
			return
		}
	}

	// Fetch from inventory-api by barcode
	invURL := fmt.Sprintf("%s/v1/%s/inventory/items?barcode=%s&limit=1", inventoryURL(), tenantSlug, barcode)
	body, err := doInventoryGET(r.Context(), invURL, "")
	if err != nil {
		h.log.Warn("barcode lookup: inventory-api error", zap.String("barcode", barcode), zap.Error(err))
		jsonError(w, "item not found", http.StatusNotFound)
		return
	}

	var wrapper struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil || len(wrapper.Data) == 0 {
		jsonError(w, "item not found", http.StatusNotFound)
		return
	}

	item := wrapper.Data[0]
	sku, _ := item["sku"].(string)

	// Merge price from local override
	if sku != "" {
		override, _ := h.client.POSCatalogOverride.Query().
			Where(
				entoverride.TenantID(tid),
				entoverride.InventorySku(sku),
			).First(r.Context())
		if override != nil && override.SellingPrice != nil && *override.SellingPrice > 0 {
			item["price"] = *override.SellingPrice
		}
	}

	payload, _ := json.Marshal(map[string]any{"data": item})

	if h.redis != nil && len(payload) > 0 {
		h.redis.Set(context.Background(), cacheKey, payload, barcodeCacheTTL)
	}

	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(payload)
}
