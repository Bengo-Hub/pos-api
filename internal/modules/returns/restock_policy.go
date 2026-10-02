package returns

import (
	"context"
	"sort"

	"github.com/google/uuid"

	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
)

// Restock policy: whether a completed return's goods go back into sellable stock. Goods that
// cannot be resold (damaged, defective, expired) are written off by default instead of being
// restocked; the outlet can change the list, and a manager can override it per return at
// completion. Stored in OutletSetting.metadata, no schema field.

// MetaKeyNoRestockReasons is the OutletSetting.metadata key holding the reason codes whose
// returns are NOT restocked.
const MetaKeyNoRestockReasons = "return_no_restock_reasons"

// DefaultNoRestockReasons applies when the outlet has never saved the setting.
var DefaultNoRestockReasons = []string{
	string(posreturn.ReasonCodeDamaged),
	string(posreturn.ReasonCodeDefective),
	string(posreturn.ReasonCodeExpired),
}

// Restock decisions stored on the return (metadata restock_decision).
const (
	mdRestockDecision   = "restock_decision"
	decisionRestock     = "restock"
	decisionWriteOff    = "write_off"
	restockNotRestocked = "not_restocked"
)

// NoRestockReasons reads the outlet's list from setting metadata, falling back to the default
// when the key was never saved. An explicitly saved empty list means "restock everything".
func NoRestockReasons(meta map[string]any) []string {
	raw, ok := meta[MetaKeyNoRestockReasons]
	if !ok || raw == nil {
		return append([]string(nil), DefaultNoRestockReasons...)
	}
	out := []string{}
	switch v := raw.(type) {
	case []string:
		out = append(out, v...)
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// SanitizeNoRestockReasons keeps only known reason codes, de-duplicated and sorted, so a stored
// list can never hold a value the policy check would silently ignore.
func SanitizeNoRestockReasons(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, r := range in {
		if reasonCodePtr(r) != nil && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// restockByPolicy decides whether a return with this reason code goes back into stock under
// the outlet's policy. No reason code (an Edit-Sale correction, or a cashier who skipped the
// field) restocks: only an explicitly unsellable reason writes goods off. If the setting
// cannot be read, the default list applies.
func (s *Service) restockByPolicy(ctx context.Context, outletID uuid.UUID, reasonCode *posreturn.ReasonCode) bool {
	if reasonCode == nil || *reasonCode == "" {
		return true
	}
	var meta map[string]any
	if setting, err := s.client.OutletSetting.Query().Where(entoutletsetting.OutletID(outletID)).Only(ctx); err == nil {
		meta = setting.Metadata
	}
	for _, r := range NoRestockReasons(meta) {
		if r == string(*reasonCode) {
			return false
		}
	}
	return true
}
