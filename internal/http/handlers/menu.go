package handlers

import (
	"context"
	"net/http"
	"sort"
	"strings"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	enttenant "github.com/bengobox/pos-service/internal/ent/tenant"
)

// MenuHandler renders a tenant-branded, printable customer menu document for an outlet.
//
// The document is regenerated on every request so it always reflects the live catalog
// (newly added / deactivated / sold-out items). It reuses CatalogHandler.assembleMenuItems
// for item assembly (never re-implements the inventory fetch) and the receiptBrand /
// branding pattern for tenant brand colour + name.
//
// PUBLIC/TOKENLESS NOTE: the QR code encodes a URL the customer scans with a phone, so
// that target MUST be reachable without a staff JWT. This handler is therefore intended to
// be registered in the router's PUBLIC group (the same group as PublicOutletHandler /
// pinAuth.ListStaff, which uses httpware.TenantV2 with URLParam tenantID and NO RequireAuth).
// Because that group does not run OutletContextMiddleware, this handler resolves the outlet
// (and its use_case + tenant_slug) directly from the path param instead of OutletFromContext.
type MenuHandler struct {
	log     *zap.Logger
	client  *ent.Client
	cache   *sharedcache.Aside // tenant branding cache (auth-api source) — same as ReceiptHandler
	authURL string
	catalog *CatalogHandler // reused for assembleMenuItems (no inventory-fetch duplication)
}

// NewMenuHandler creates a MenuHandler. Wire it exactly like ReceiptHandler in app.go,
// passing the shared tenantCache, cfg.Auth.ServiceURL, and the existing CatalogHandler.
func NewMenuHandler(log *zap.Logger, client *ent.Client, cache *sharedcache.Aside, authURL string, catalog *CatalogHandler) *MenuHandler {
	return &MenuHandler{log: log, client: client, cache: cache, authURL: authURL, catalog: catalog}
}

// branding mirrors ReceiptHandler.branding: best-effort tenant logo/name/primary-colour from
// the shared cache. Returns a zero-value receiptBrand if anything is unavailable.
// (receiptBrand is reused from receipt.go — same package — to avoid a duplicate brand type.)
func (h *MenuHandler) branding(ctx context.Context, tenantID uuid.UUID) receiptBrand {
	var b receiptBrand
	if h.cache == nil || h.authURL == "" {
		return b
	}
	t, err := h.client.Tenant.Query().Where(enttenant.ID(tenantID)).Only(ctx)
	if err != nil {
		return b
	}
	b.CompanyName = t.Name
	td, err := sharedcache.GetTenantDetails(ctx, h.cache, h.authURL, t.Slug, sharedcache.DefaultTenantTTL)
	if err != nil {
		return b
	}
	tb := sharedcache.GetTenantBranding(td)
	if tb.Name != "" {
		b.CompanyName = tb.Name
	}
	b.LogoURL = tb.LogoURL
	b.PrimaryColor = tb.PrimaryColor
	return b
}

// menuGroup is one category section in the rendered menu.
type menuGroup struct {
	CategoryName string
	Items        []catalogItemDTO
}

// GetMenuHTML handles GET /{tenantID}/pos/outlets/{outletID}/menu.html
//
// It resolves the outlet (path param), tenant branding, assembles the active+available
// catalog items via the shared CatalogHandler.assembleMenuItems, groups them by category,
// generates a QR code that encodes the public menu URL, and renders the branded HTML.
//
// The QR target is the menu's own absolute URL (so scanning re-opens this page). When the
// service sits behind a proxy, set PUBLIC_BASE_URL so the QR encodes the externally
// reachable origin instead of the internal request host.
func (h *MenuHandler) GetMenuHTML(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	outletID, err := uuid.Parse(chi.URLParam(r, "outletID"))
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}

	// Resolve outlet directly from the path (public group has no OutletContextMiddleware).
	outlet, err := h.client.Outlet.Query().
		Where(entoutlet.ID(outletID), entoutlet.TenantID(tid), entoutlet.StatusNEQ("archived")).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "outlet not found", http.StatusNotFound)
			return
		}
		h.log.Error("menu: outlet lookup failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	useCase := ""
	if outlet.UseCase != nil {
		useCase = *outlet.UseCase
	}
	tenantSlug := outlet.TenantSlug // Outlet carries tenant_slug directly — no claims needed.

	brand := h.branding(ctx, tid)
	tenantName := brand.CompanyName
	if tenantName == "" {
		tenantName = outlet.TenantSlug
	}

	// Assemble the live catalog (active+available only). No list filters — the menu shows everything.
	items, err := h.catalog.assembleMenuItems(ctx, tid, tenantSlug, &outletID, useCase, menuAssemblyFilters{})
	if err != nil {
		h.log.Error("menu: assemble items failed", zap.Error(err))
		jsonError(w, "failed to build menu", http.StatusBadGateway)
		return
	}

	groups := groupMenuItems(items)

	// QR encodes the public menu URL = (PUBLIC_BASE_URL or request origin) + request path.
	menuURL := publicMenuURL(r)
	qrDataURI, qrErr := qrPNGDataURI(menuURL, 256)
	if qrErr != nil {
		// Non-fatal: render the menu without a QR rather than failing the whole document.
		h.log.Warn("menu: QR generation failed", zap.Error(qrErr))
		qrDataURI = ""
	}

	htmlOut := generateMenuHTML(groups, brand, tenantName, outlet.Name, menuURL, qrDataURI)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="menu.html"`)
	_, _ = w.Write(htmlOut)
}

// groupMenuItems buckets active+available items by category, skipping anything not sellable.
// Categories are returned alphabetically; items within a category keep DisplayOrder then name.
// Items with no category fall under an "Other" bucket so they are never silently dropped.
func groupMenuItems(items []catalogItemDTO) []menuGroup {
	const uncategorised = "Other"
	byCat := map[string][]catalogItemDTO{}
	for _, it := range items {
		// Only active + available items appear on a customer-facing menu.
		if !it.IsActive || !it.IsAvailable {
			continue
		}
		cat := strings.TrimSpace(it.CategoryName)
		if cat == "" {
			cat = uncategorised
		}
		byCat[cat] = append(byCat[cat], it)
	}

	cats := make([]string, 0, len(byCat))
	for c := range byCat {
		cats = append(cats, c)
	}
	sort.Slice(cats, func(i, j int) bool {
		// Keep the catch-all "Other" bucket last.
		if cats[i] == uncategorised {
			return false
		}
		if cats[j] == uncategorised {
			return true
		}
		return strings.ToLower(cats[i]) < strings.ToLower(cats[j])
	})

	groups := make([]menuGroup, 0, len(cats))
	for _, c := range cats {
		list := byCat[c]
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].DisplayOrder != list[j].DisplayOrder {
				return list[i].DisplayOrder < list[j].DisplayOrder
			}
			return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
		})
		groups = append(groups, menuGroup{CategoryName: c, Items: list})
	}
	return groups
}
