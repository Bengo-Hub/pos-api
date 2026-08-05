package saleedit

import (
	"context"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/modules/orders"
)

// policy is the resolved routing decision for one Edit call — checks tenant eTIMS-integration
// status, this specific order's live fiscalization state, and its AR nature, exactly once per
// call, so the orchestrator branches on it instead of scattering these checks across the code.
type policy struct {
	fiscalized bool
	invoiceID  string
	onAccount  bool
}

// resolvePolicy is the ONE place Edit Sale decides in-place vs return vs addendum. Fast
// pre-check: if the tenant isn't eTIMS-integrated at all, every one of its orders is
// definitionally never fiscalized — skip the per-order treasury round-trip entirely.
func (s *Service) resolvePolicy(ctx context.Context, tenantID uuid.UUID, tenantSlug string, orderID uuid.UUID) policy {
	p := policy{onAccount: orders.SettledOnAccount(ctx, s.client, tenantID, orderID)}

	if s.treasuryClient == nil {
		return p
	}
	if profile, err := s.treasuryClient.GetTaxProfile(ctx, tenantSlug); err == nil && profile != nil && !profile.EtimsActivated {
		return p // tenant not eTIMS-integrated — never fiscalized, skip the per-order lookup
	}
	fiscalized, invoiceID, _ := orders.IsFiscalized(ctx, s.treasuryClient, tenantSlug, orderID)
	p.fiscalized = fiscalized
	p.invoiceID = invoiceID
	return p
}
