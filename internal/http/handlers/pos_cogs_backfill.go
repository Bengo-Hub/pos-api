package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/modules/inventory"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// RecipeCOGSBackfillHandler is the platform-owner-only, one-time recovery tool that posts the
// missing COGS for every historical completed order whose RECIPE-type lines contributed zero
// cost at sale time — the bug fixed by inventory-api's RecalculateRecipeCosts write-through onto
// Item.CostPrice (recipe items previously had no cost_price at all, so PostCOGS silently no-oped
// for them; see recipe-costing-unit-conversion notes). No historical cost snapshot exists per
// order/line, so this necessarily uses TODAY's recipe ingredient costs applied retroactively — an
// approximation, not a true historical figure, and clearly labelled as such in every response.
type RecipeCOGSBackfillHandler struct {
	client    *ent.Client
	inventory *inventory.Client
	treasury  *treasury.Client
	log       *zap.Logger
}

func NewRecipeCOGSBackfillHandler(client *ent.Client, inv *inventory.Client, tr *treasury.Client, log *zap.Logger) *RecipeCOGSBackfillHandler {
	return &RecipeCOGSBackfillHandler{client: client, inventory: inv, treasury: tr, log: log.Named("recipe-cogs-backfill")}
}

// RegisterRoutes mounts the tool. The caller MUST wrap this router group in requirePlatformOwner
// — this handler does not re-check itself (matches every other manual ops recovery tool in this
// codebase, which relies on the router-level gate).
func (h *RecipeCOGSBackfillHandler) RegisterRoutes(r chi.Router) {
	r.Post("/recovery/recipe-cogs-backfill", h.Backfill)
}

type tenantBackfillSummary struct {
	TenantID       string  `json:"tenant_id"`
	OrdersScanned  int     `json:"orders_scanned"`
	OrdersAffected int     `json:"orders_affected"`
	TotalMissing   float64 `json:"total_missing_cogs"`
	Currency       string  `json:"currency"`
	Posted         int     `json:"posted,omitempty"` // only meaningful when dry_run=false
	Errors         int     `json:"errors,omitempty"`
}

// Backfill handles POST /api/v1/recovery/recipe-cogs-backfill  body: { dry_run? } (default true).
// Fleet-wide (every tenant with at least one completed order) — runs in a background goroutine
// detached from the request context (this can take a while across the whole fleet, well past the
// router's 30s request timeout) and logs a structured per-tenant summary + a fleet-wide total via
// zap as it completes each tenant. Check pod logs for the results; this endpoint only confirms
// the job started. Idempotent either way — see PostCOGS' own (tenant, reference_type,
// reference_id) guard — so re-running (e.g. after reviewing a dry run) never double-posts.
func (h *RecipeCOGSBackfillHandler) Backfill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DryRun *bool `json:"dry_run"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	dryRun := true
	if body.DryRun != nil {
		dryRun = *body.DryRun
	}

	go h.run(dryRun)

	respondJSON(w, http.StatusAccepted, map[string]any{
		"status":  "started",
		"dry_run": dryRun,
		"note":    "Fleet-wide job running in the background — check pod logs (logger name recipe-cogs-backfill) for per-tenant results and the final fleet-wide total. Costs are TODAY's recipe ingredient costs applied to historical orders (an approximation — no historical cost snapshot exists).",
	})
}

func (h *RecipeCOGSBackfillHandler) run(dryRun bool) {
	ctx := context.Background()
	h.log.Info("recipe COGS backfill: starting", zap.Bool("dry_run", dryRun))

	var tenantRows []struct {
		TenantID uuid.UUID `json:"tenant_id"`
	}
	if err := h.client.POSOrder.Query().
		Where(posorder.StatusEQ("completed")).
		GroupBy(posorder.FieldTenantID).
		Scan(ctx, &tenantRows); err != nil {
		h.log.Error("recipe COGS backfill: failed to list tenants", zap.Error(err))
		return
	}

	fleetMissing := 0.0
	fleetOrdersAffected := 0
	fleetPosted := 0
	for _, row := range tenantRows {
		summary := h.runTenant(ctx, row.TenantID, dryRun)
		h.log.Info("recipe COGS backfill: tenant done",
			zap.String("tenant_id", summary.TenantID),
			zap.Int("orders_scanned", summary.OrdersScanned),
			zap.Int("orders_affected", summary.OrdersAffected),
			zap.Float64("total_missing_cogs", summary.TotalMissing),
			zap.String("currency", summary.Currency),
			zap.Int("posted", summary.Posted),
			zap.Int("errors", summary.Errors),
		)
		fleetMissing += summary.TotalMissing
		fleetOrdersAffected += summary.OrdersAffected
		fleetPosted += summary.Posted
	}

	h.log.Info("recipe COGS backfill: FLEET-WIDE COMPLETE",
		zap.Bool("dry_run", dryRun),
		zap.Int("tenants_scanned", len(tenantRows)),
		zap.Int("orders_affected", fleetOrdersAffected),
		zap.Float64("total_missing_cogs_kes_equivalent", fleetMissing),
		zap.Int("posted", fleetPosted),
	)
}

func (h *RecipeCOGSBackfillHandler) runTenant(ctx context.Context, tenantID uuid.UUID, dryRun bool) tenantBackfillSummary {
	sum := tenantBackfillSummary{TenantID: tenantID.String(), Currency: "KES"}

	recipeCosts, err := h.inventory.ListRecipeCosts(ctx, tenantID.String())
	if err != nil {
		h.log.Warn("recipe COGS backfill: failed to load recipe costs, skipping tenant",
			zap.String("tenant_id", tenantID.String()), zap.Error(err))
		sum.Errors++
		return sum
	}
	if len(recipeCosts) == 0 {
		return sum // no recipe items for this tenant — nothing to backfill
	}

	const pageSize = 200
	for offset := 0; ; offset += pageSize {
		orders, err := h.client.POSOrder.Query().
			Where(posorder.TenantID(tenantID), posorder.StatusEQ("completed")).
			WithLines().
			Order(ent.Asc(posorder.FieldCreatedAt)).
			Limit(pageSize).
			Offset(offset).
			All(ctx)
		if err != nil {
			h.log.Warn("recipe COGS backfill: failed to page orders",
				zap.String("tenant_id", tenantID.String()), zap.Int("offset", offset), zap.Error(err))
			sum.Errors++
			break
		}
		if len(orders) == 0 {
			break
		}
		sum.OrdersScanned += len(orders)
		if orders[0].Currency != "" {
			sum.Currency = orders[0].Currency
		}

		for _, o := range orders {
			var missing float64
			for _, l := range o.Edges.Lines {
				cost, ok := recipeCosts[l.Sku]
				if !ok {
					continue
				}
				activeQty := l.Quantity
				if l.VoidedQty != nil {
					activeQty -= *l.VoidedQty
				}
				if activeQty > 0 {
					missing += activeQty * cost
				}
			}
			if missing <= 0.009 {
				continue
			}
			sum.OrdersAffected++
			sum.TotalMissing += missing

			if dryRun {
				continue
			}
			var outletID string
			if o.OutletID != uuid.Nil {
				outletID = o.OutletID.String()
			}
			resp, err := h.treasury.PostCOGSBackfill(ctx, tenantID.String(), treasury.COGSBackfillRequest{
				ReferenceID: o.ID.String(),
				Amount:      missing,
				Currency:    o.Currency,
				Description: fmt.Sprintf("Recipe COGS backfill for order %s", o.OrderNumber),
				OutletID:    outletID,
			})
			if err != nil {
				h.log.Warn("recipe COGS backfill: post failed",
					zap.String("tenant_id", tenantID.String()), zap.String("order", o.OrderNumber), zap.Error(err))
				sum.Errors++
				continue
			}
			if resp != nil && resp.Posted {
				sum.Posted++
			}
		}

		if len(orders) < pageSize {
			break
		}
		// Be a reasonable citizen against inventory/treasury during a fleet-wide, since-inception
		// run — this is a one-time recovery job, not a hot path.
		time.Sleep(50 * time.Millisecond)
	}

	return sum
}
