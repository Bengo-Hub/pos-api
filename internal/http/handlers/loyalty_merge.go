package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/customers"
)

// LoyaltyMergeHandler is the platform-owner-only, one-off tool that merges duplicate loyalty
// accounts: accounts of one tenant whose phones are the same number written differently
// ("0712...", "254712...", "+254712..."). See customers.MergeDuplicateAccounts for the rules.
//
// Dry run by default. The real run needs "dry_run": false and either a tenant_id or
// "all_tenants": true in the body. Safe to run again: merged shells are skipped, so a second
// real run reports no groups.
type LoyaltyMergeHandler struct {
	client *ent.Client
	log    *zap.Logger
}

// NewLoyaltyMergeHandler builds the handler.
func NewLoyaltyMergeHandler(client *ent.Client, log *zap.Logger) *LoyaltyMergeHandler {
	return &LoyaltyMergeHandler{client: client, log: log.Named("loyalty-merge")}
}

// RegisterRoutes mounts the tool. The caller MUST wrap this router group in
// requirePlatformOwner; this handler does not check the caller itself.
func (h *LoyaltyMergeHandler) RegisterRoutes(r chi.Router) {
	r.Post("/recovery/loyalty-merge-duplicates", h.Merge)
}

// mergeRequest is the body of the merge tool.
type mergeRequest struct {
	TenantID   *string `json:"tenant_id"`
	DryRun     *bool   `json:"dry_run"`
	AllTenants bool    `json:"all_tenants"`
}

// parseMergeRequest reads and checks the body. Any body that is not exactly the expected JSON
// (a wrong type, an unknown field, trailing data) is refused, so a typo can never turn into a
// real run over every tenant. An empty body is a dry run over every tenant. A real run needs
// either a tenant_id or "all_tenants": true, never both.
func parseMergeRequest(r *http.Request) (customers.MergeOptions, string) {
	var body mergeRequest
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			return customers.MergeOptions{}, "invalid request body"
		}
		if dec.More() {
			return customers.MergeOptions{}, "invalid request body"
		}
	}
	opts := customers.MergeOptions{Apply: body.DryRun != nil && !*body.DryRun}
	hasTenant := body.TenantID != nil && *body.TenantID != ""
	if hasTenant {
		tid, err := uuid.Parse(*body.TenantID)
		if err != nil || tid == uuid.Nil {
			return customers.MergeOptions{}, "invalid tenant_id"
		}
		opts.TenantID = &tid
	}
	if hasTenant && body.AllTenants {
		return customers.MergeOptions{}, "give either tenant_id or all_tenants, not both"
	}
	if opts.Apply && !hasTenant && !body.AllTenants {
		return customers.MergeOptions{}, "a real run needs a tenant_id or \"all_tenants\": true"
	}
	return opts, ""
}

// Merge handles POST /api/v1/recovery/loyalty-merge-duplicates
// body: { tenant_id?: string, all_tenants?: bool, dry_run?: bool } (dry_run defaults to true).
// One tenant when tenant_id is given, every tenant otherwise; a real run must name the tenant
// or say all_tenants. Answers with the report: per group the tenant, a masked phone, the
// account ids and balances, the survivor and why.
func (h *LoyaltyMergeHandler) Merge(w http.ResponseWriter, r *http.Request) {
	opts, problem := parseMergeRequest(r)
	if problem != "" {
		jsonError(w, problem, http.StatusBadRequest)
		return
	}

	rep, err := customers.MergeDuplicateAccounts(r.Context(), h.client, opts, time.Now())
	if err != nil {
		h.log.Error("loyalty merge failed", zap.Bool("dry_run", !opts.Apply), zap.Error(err))
		jsonError(w, "loyalty merge failed", http.StatusInternalServerError)
		return
	}
	for _, g := range rep.Groups {
		if g.Status == customers.MergeStatusRefused {
			h.log.Warn("loyalty merge refused a group",
				zap.String("tenant_id", g.TenantID.String()), zap.String("survivor_id", g.SurvivorID.String()),
				zap.String("reason", g.Error))
		}
	}
	h.log.Info("loyalty merge done",
		zap.Bool("dry_run", rep.DryRun), zap.Int("tenants_scanned", rep.Tenants),
		zap.Int("groups", len(rep.Groups)), zap.Int("merged", rep.Merged), zap.Int("refused", rep.Refused))
	respondJSON(w, http.StatusOK, rep)
}
