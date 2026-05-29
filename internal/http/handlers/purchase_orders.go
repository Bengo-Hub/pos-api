package handlers

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
)

// PurchaseOrdersHandler proxies purchase-order requests to inventory-api,
// adding tenant auth headers so pos-ui never calls inventory-api directly.
type PurchaseOrdersHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewPurchaseOrdersHandler(log *zap.Logger, client *ent.Client) *PurchaseOrdersHandler {
	return &PurchaseOrdersHandler{log: log, client: client}
}

// proxyToInventory forwards an HTTP request to inventory-api and streams the response back.
// tenantSlug is placed in the path; outletID and tenant headers are forwarded.
func (h *PurchaseOrdersHandler) proxyToInventory(w http.ResponseWriter, r *http.Request, method, upstreamPath string, body io.Reader) {
	upstream := fmt.Sprintf("%s%s", inventoryURL(), upstreamPath)

	// Forward query params from the incoming request.
	if r.URL.RawQuery != "" {
		upstream += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), method, upstream, body)
	if err != nil {
		h.log.Error("purchase-orders proxy: build request failed", zap.Error(err))
		jsonError(w, "proxy error", http.StatusInternalServerError)
		return
	}

	if k := serviceAPIKey(); k != "" {
		req.Header.Set("X-API-Key", k)
	}
	if tid := r.Header.Get("X-Tenant-ID"); tid != "" {
		req.Header.Set("X-Tenant-ID", tid)
	}
	if slug := r.Header.Get("X-Tenant-Slug"); slug != "" {
		req.Header.Set("X-Tenant-Slug", slug)
	}
	if oid := r.Header.Get("X-Outlet-ID"); oid != "" {
		req.Header.Set("X-Outlet-ID", oid)
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.log.Error("purchase-orders proxy: upstream call failed", zap.Error(err), zap.String("upstream", upstream))
		jsonError(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// inventoryPOPath returns the inventory-api path for purchase orders given a tenant slug.
func inventoryPOPath(tenantSlug, suffix string) string {
	return fmt.Sprintf("/v1/%s/inventory/purchase-orders%s", url.PathEscape(tenantSlug), suffix)
}

// tenantSlugFromRequest extracts X-Tenant-Slug (preferred) or falls back to the URL tenantID param.
func tenantSlugFromRequest(r *http.Request) string {
	if s := r.Header.Get("X-Tenant-Slug"); s != "" {
		return s
	}
	return chi.URLParam(r, "tenantID")
}

// List handles GET /{tenantID}/pos/purchase-orders
func (h *PurchaseOrdersHandler) List(w http.ResponseWriter, r *http.Request) {
	slug := tenantSlugFromRequest(r)
	h.proxyToInventory(w, r, http.MethodGet, inventoryPOPath(slug, ""), nil)
}

// Create handles POST /{tenantID}/pos/purchase-orders
func (h *PurchaseOrdersHandler) Create(w http.ResponseWriter, r *http.Request) {
	slug := tenantSlugFromRequest(r)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonError(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	h.proxyToInventory(w, r, http.MethodPost, inventoryPOPath(slug, ""), bytes.NewReader(body))
}

// Get handles GET /{tenantID}/pos/purchase-orders/{id}
func (h *PurchaseOrdersHandler) Get(w http.ResponseWriter, r *http.Request) {
	slug := tenantSlugFromRequest(r)
	poID := chi.URLParam(r, "id")
	h.proxyToInventory(w, r, http.MethodGet, inventoryPOPath(slug, "/"+url.PathEscape(poID)), nil)
}
