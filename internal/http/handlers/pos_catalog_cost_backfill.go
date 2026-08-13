package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	catalogmodule "github.com/bengobox/pos-service/internal/modules/catalog"
	"github.com/bengobox/pos-service/internal/modules/inventory"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// CatalogCostBackfillHandler is the platform-owner-only, one-time recovery tool that re-populates
// pos-api's local POSCatalogOverride cost cache (metadata["cost_price"], read by
// resolveUnitCostsBySKU/orders.CatalogCacheBySKU — the SAME cache every gross-profit report reads)
// from inventory-api's current, authoritative Item.cost_price.
//
// Closes the gap left by commit ef9e882 (2026-08-10, "resolve gross-profit cost from local cache
// instead of a live full-catalog fetch"): that fix correctly stopped resolving cost via a live,
// rate-limited inventory-api catalog walk on every report request and switched to this local
// event-driven cache — but it never backfilled the cache for SKUs whose last relevant
// inventory.item.* event landed before/during that incident, so those SKUs were left permanently
// stuck at cost=0 with no self-healing path (a genuinely stale cache entry looks identical to "no
// data yet" — see resolveUnitCostsBySKU's/report_attribution.go's doc comments). Confirmed live
// against boi-enterprises 2026-08-13: dozens of high-volume SKUs (e.g. "A16 128/4 SAMSUNG") showed
// unit_cost=0/margin_pct=100 in /pos/reports/most-profitable despite inventory-api's live Item
// record carrying a real, non-zero cost_price for the exact same SKU.
//
// Modeled directly on RecipeCOGSBackfillHandler (pos_cogs_backfill.go) — same shape: platform-owner
// gated (the router wraps this in requirePlatformOwner, matching that handler — this one does not
// re-check), dry-run by default, idempotent (re-running only re-merges the same cost_price values;
// see catalog.upsertMetadataMerge), safe to re-run after reviewing a dry run.
type CatalogCostBackfillHandler struct {
	client    *ent.Client
	inventory *inventory.Client
	db        *sql.DB
	log       *zap.Logger
}

func NewCatalogCostBackfillHandler(client *ent.Client, inv *inventory.Client, db *sql.DB, log *zap.Logger) *CatalogCostBackfillHandler {
	return &CatalogCostBackfillHandler{client: client, inventory: inv, db: db, log: log.Named("catalog-cost-backfill")}
}

// RegisterRoutes mounts the tool. The caller MUST wrap this router group in requirePlatformOwner —
// this handler does not re-check itself (matches recipeCOGSBackfill/backupDest, the other manual
// ops recovery tools registered in the same router group).
func (h *CatalogCostBackfillHandler) RegisterRoutes(r chi.Router) {
	r.Post("/recovery/catalog-cost-backfill", h.Backfill)
}

type catalogCostTenantSummary struct {
	TenantID      string `json:"tenant_id"`
	SKUsScanned   int    `json:"skus_scanned"`
	SKUsCorrected int    `json:"skus_corrected"` // dry_run=true: WOULD correct; false: DID correct
	Errors        int    `json:"errors,omitempty"`
}

// Backfill handles POST /api/v1/recovery/catalog-cost-backfill
// body: { tenant_id?: string, dry_run?: bool } (dry_run defaults true).
// Scopes to ONE tenant when tenant_id is given; fleet-wide (every tenant with at least one
// completed POS order — mirrors RecipeCOGSBackfillHandler's tenant discovery) when omitted. Runs in
// a background goroutine detached from the request context (a fleet-wide, whole-catalog walk can
// take a while); check pod logs (logger name catalog-cost-backfill) for per-tenant results.
func (h *CatalogCostBackfillHandler) Backfill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TenantID *string `json:"tenant_id"`
		DryRun   *bool   `json:"dry_run"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	dryRun := true
	if body.DryRun != nil {
		dryRun = *body.DryRun
	}

	var scopeTenant *uuid.UUID
	if body.TenantID != nil && *body.TenantID != "" {
		tid, err := uuid.Parse(*body.TenantID)
		if err != nil {
			jsonError(w, "invalid tenant_id", http.StatusBadRequest)
			return
		}
		scopeTenant = &tid
	}

	go h.run(scopeTenant, dryRun)

	respondJSON(w, http.StatusAccepted, map[string]any{
		"status":  "started",
		"dry_run": dryRun,
		"note":    "Running in the background — check pod logs (logger name catalog-cost-backfill) for per-tenant results and the final total.",
	})
}

func (h *CatalogCostBackfillHandler) run(scopeTenant *uuid.UUID, dryRun bool) {
	ctx := context.Background()
	h.log.Info("catalog-cost backfill: starting", zap.Bool("dry_run", dryRun), zap.Any("scope_tenant", scopeTenant))

	var tenantIDs []uuid.UUID
	if scopeTenant != nil {
		tenantIDs = []uuid.UUID{*scopeTenant}
	} else {
		var tenantRows []struct {
			TenantID uuid.UUID `json:"tenant_id"`
		}
		if err := h.client.POSOrder.Query().
			Where(posorder.StatusEQ("completed")).
			GroupBy(posorder.FieldTenantID).
			Scan(ctx, &tenantRows); err != nil {
			h.log.Error("catalog-cost backfill: failed to list tenants", zap.Error(err))
			return
		}
		for _, row := range tenantRows {
			tenantIDs = append(tenantIDs, row.TenantID)
		}
	}

	var fleetScanned, fleetCorrected int
	for _, tid := range tenantIDs {
		summary := h.runTenant(ctx, tid, dryRun)
		h.log.Info("catalog-cost backfill: tenant done",
			zap.String("tenant_id", summary.TenantID),
			zap.Int("skus_scanned", summary.SKUsScanned),
			zap.Int("skus_corrected", summary.SKUsCorrected),
			zap.Int("errors", summary.Errors),
		)
		fleetScanned += summary.SKUsScanned
		fleetCorrected += summary.SKUsCorrected
		// Be a reasonable citizen against inventory-api across a fleet-wide run — this is a
		// one-time recovery job, not a hot path.
		if len(tenantIDs) > 1 {
			time.Sleep(200 * time.Millisecond)
		}
	}

	h.log.Info("catalog-cost backfill: COMPLETE",
		zap.Bool("dry_run", dryRun),
		zap.Int("tenants_scanned", len(tenantIDs)),
		zap.Int("skus_scanned", fleetScanned),
		zap.Int("skus_corrected", fleetCorrected),
	)
}

func (h *CatalogCostBackfillHandler) runTenant(ctx context.Context, tenantID uuid.UUID, dryRun bool) catalogCostTenantSummary {
	sum := catalogCostTenantSummary{TenantID: tenantID.String()}

	liveCosts, err := h.inventory.ListCatalogCosts(ctx, tenantID.String())
	if err != nil {
		h.log.Warn("catalog-cost backfill: failed to load inventory-api catalog, skipping tenant",
			zap.String("tenant_id", tenantID.String()), zap.Error(err))
		sum.Errors++
		return sum
	}
	sum.SKUsScanned = len(liveCosts)
	if len(liveCosts) == 0 {
		return sum
	}

	skus := make([]string, 0, len(liveCosts))
	for sku := range liveCosts {
		skus = append(skus, sku)
	}
	// Current cache state — used only to count how many SKUs a dry run WOULD correct (a real run
	// re-merges every positive-cost SKU unconditionally, since upsertMetadataMerge is a cheap,
	// idempotent no-op when the value is already correct).
	cachedCosts := orders.CatalogCostBySKU(ctx, h.client, tenantID, skus)

	for sku, item := range liveCosts {
		if item.CostPrice <= 0 {
			continue // never overwrite a real cached cost with an unpriced/zero inventory-api row
		}
		if cachedCosts[sku] == item.CostPrice {
			continue // already correct — not counted as "corrected"
		}
		sum.SKUsCorrected++
		if dryRun {
			continue
		}
		if err := catalogmodule.BackfillCatalogCost(ctx, h.db, tenantID, sku, catalogmodule.BackfillCatalogCostFields{
			CostPrice:    item.CostPrice,
			Manufacturer: item.Manufacturer,
			CategoryName: item.CategoryName,
		}); err != nil {
			h.log.Warn("catalog-cost backfill: upsert failed",
				zap.String("tenant_id", tenantID.String()), zap.String("sku", sku), zap.Error(err))
			sum.Errors++
		}
	}
	return sum
}
