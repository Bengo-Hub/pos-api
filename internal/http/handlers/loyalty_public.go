package handlers

import (
	"context"
	"net/http"
	"sort"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entlp "github.com/bengobox/pos-service/internal/ent/loyaltyprogram"
)

// The public loyalty page of a tenant website shows how the programme works before anyone signs
// in. It reads only the programme's own terms (rates and level thresholds), never an account, so
// it needs no token. Before this the site called the staff route, every visitor got a 401, and
// the site's 401 handler sent signed-in customers into an endless SSO logout loop.

// LoyaltyTierView is one level of a programme, from tier_thresholds (name to lifetime points).
type LoyaltyTierView struct {
	Name      string `json:"name"`
	MinPoints int    `json:"min_points"`
}

// PublicLoyaltyProgramView is what a visitor may see about the active programme.
type PublicLoyaltyProgramView struct {
	ID              uuid.UUID         `json:"id"`
	Name            string            `json:"name"`
	Description     string            `json:"description,omitempty"`
	EarnRate        float64           `json:"earn_rate"`
	RedeemRate      float64           `json:"redeem_rate"`
	MinRedeemPoints int               `json:"min_redeem_points"`
	Tiers           []LoyaltyTierView `json:"tiers"`
}

// loyaltyTiers turns tier_thresholds into levels ordered by the points they start at.
func loyaltyTiers(thresholds map[string]any) []LoyaltyTierView {
	tiers := make([]LoyaltyTierView, 0, len(thresholds))
	for name, v := range thresholds {
		var min int
		switch n := v.(type) {
		case float64:
			min = int(n)
		case int:
			min = n
		case int64:
			min = int(n)
		default:
			continue
		}
		tiers = append(tiers, LoyaltyTierView{Name: name, MinPoints: min})
	}
	sort.Slice(tiers, func(i, j int) bool {
		if tiers[i].MinPoints != tiers[j].MinPoints {
			return tiers[i].MinPoints < tiers[j].MinPoints
		}
		return tiers[i].Name < tiers[j].Name
	})
	return tiers
}

// loyaltyTierFor is the highest level whose threshold the lifetime points reach ("" when none).
func loyaltyTierFor(tiers []LoyaltyTierView, lifetime int) string {
	name := ""
	for _, t := range tiers {
		if lifetime >= t.MinPoints {
			name = t.Name
		}
	}
	return name
}

// activeLoyaltyProgram is the tenant's active programme (the one accounts join), or nil.
func activeLoyaltyProgram(ctx context.Context, db *ent.Client, tid uuid.UUID) (*ent.LoyaltyProgram, error) {
	p, err := db.LoyaltyProgram.Query().
		Where(entlp.TenantID(tid), entlp.IsActive(true)).
		Order(ent.Asc(entlp.FieldCreatedAt)).
		First(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	return p, err
}

// PublicProgram handles GET /{tenantID}/pos/loyalty/program (no auth): the active programme's
// terms and levels, or data null when the tenant runs none.
func (h *LoyaltyHandler) PublicProgram(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	p, err := activeLoyaltyProgram(r.Context(), h.db, tid)
	if err != nil {
		h.log.Error("public loyalty program failed", zap.Error(err))
		jsonError(w, "failed to load programme", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	if p == nil {
		jsonOK(w, map[string]any{"data": nil})
		return
	}
	jsonOK(w, map[string]any{"data": PublicLoyaltyProgramView{
		ID:              p.ID,
		Name:            p.Name,
		Description:     p.Description,
		EarnRate:        p.EarnRate,
		RedeemRate:      p.RedeemRate,
		MinRedeemPoints: p.MinRedeemPoints,
		Tiers:           loyaltyTiers(p.TierThresholds),
	}})
}
