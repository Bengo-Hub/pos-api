// Package outletpolicy resolves per-outlet cashier/terminal policy knobs
// (OutletSetting nullable columns) against their per-use-case defaults.
//
// The resolution ladder is: outlet override (a non-nil column) → per-use-case
// default → system default. Keeping this in a small, dependency-free package
// (no ent import → no import cycle) lets both the settings handler and the
// order-scope predicate resolve the same values consistently.
//
// The per-use-case defaults mirror the confirmed product decisions:
//   - cashier_sales_visibility: hospitality => own, others => outlet
//   - auto_logout_after_sale:   hospitality + quick_service => true, others => false
//   - cashier_terminal_surface: all => full_till
package outletpolicy

import "strings"

// Canonical use-case profiles (mirror pos-ui normalizeUseCase / rbac.moduleUseCases).
const (
	UseCaseHospitality = "hospitality"
	UseCaseQuickService = "quick_service"
	UseCasePharmacy    = "pharmacy"
	UseCaseServices    = "services"
	UseCaseRetail      = "retail"
)

// Cashier sales-visibility values.
const (
	VisibilityOwn    = "own"
	VisibilityOutlet = "outlet"
)

// Cashier terminal-surface values.
const (
	SurfaceFullTill  = "full_till"
	SurfaceBillsOnly = "bills_only"
)

// hospitalityAliases / quickAliases mirror pos-ui use-case-config.ts so a raw
// outlet use_case such as "hotel"/"bar"/"cafe"/"restaurant" collapses to the
// hospitality profile (and "quick service" to quick_service) — otherwise the
// defaults would silently fall through to retail.
var (
	hospitalityAliases = []string{"hospitality", "hotel", "bar", "cafe", "restaurant"}
	quickAliases       = []string{"quick_service", "quick service"}
)

// NormalizeUseCase collapses any raw outlet use_case string onto one of the five
// canonical profiles. Unknown/empty defaults to retail (matches pos-ui).
func NormalizeUseCase(useCase string) string {
	uc := strings.ToLower(strings.TrimSpace(useCase))
	if uc == "" {
		return UseCaseRetail
	}
	for _, h := range hospitalityAliases {
		if strings.Contains(uc, h) {
			return UseCaseHospitality
		}
	}
	for _, q := range quickAliases {
		if strings.Contains(uc, q) {
			return UseCaseQuickService
		}
	}
	if strings.Contains(uc, "pharmacy") {
		return UseCasePharmacy
	}
	if strings.Contains(uc, "service") || strings.Contains(uc, "salon") ||
		strings.Contains(uc, "clinic") || strings.Contains(uc, "spa") {
		return UseCaseServices
	}
	return UseCaseRetail
}

// DefaultCashierSalesVisibility returns the per-use-case default (no override).
func DefaultCashierSalesVisibility(useCase string) string {
	if NormalizeUseCase(useCase) == UseCaseHospitality {
		return VisibilityOwn
	}
	return VisibilityOutlet
}

// DefaultAutoLogoutAfterSale returns the per-use-case default (no override).
func DefaultAutoLogoutAfterSale(useCase string) bool {
	switch NormalizeUseCase(useCase) {
	case UseCaseHospitality, UseCaseQuickService:
		return true
	default:
		return false
	}
}

// DefaultCashierTerminalSurface returns the per-use-case default (no override).
// All use cases default to the full till (POS terminal + add sale + tables shown).
func DefaultCashierTerminalSurface(useCase string) string {
	return SurfaceFullTill
}

// ResolveCashierSalesVisibility applies the ladder: outlet override → use-case default.
// An override that is not a recognized value falls back to the default (defensive).
func ResolveCashierSalesVisibility(useCase string, override *string) string {
	if override != nil {
		if v := strings.ToLower(strings.TrimSpace(*override)); v == VisibilityOwn || v == VisibilityOutlet {
			return v
		}
	}
	return DefaultCashierSalesVisibility(useCase)
}

// ResolveAutoLogoutAfterSale applies the ladder: outlet override → use-case default.
func ResolveAutoLogoutAfterSale(useCase string, override *bool) bool {
	if override != nil {
		return *override
	}
	return DefaultAutoLogoutAfterSale(useCase)
}

// ResolveCashierTerminalSurface applies the ladder: outlet override → use-case default.
func ResolveCashierTerminalSurface(useCase string, override *string) string {
	if override != nil {
		if v := strings.ToLower(strings.TrimSpace(*override)); v == SurfaceFullTill || v == SurfaceBillsOnly {
			return v
		}
	}
	return DefaultCashierTerminalSurface(useCase)
}

// ValidCashierSalesVisibility reports whether v is an accepted value (for PUT validation).
func ValidCashierSalesVisibility(v string) bool {
	return v == VisibilityOwn || v == VisibilityOutlet
}

// ValidCashierTerminalSurface reports whether v is an accepted value (for PUT validation).
func ValidCashierTerminalSurface(v string) bool {
	return v == SurfaceFullTill || v == SurfaceBillsOnly
}
