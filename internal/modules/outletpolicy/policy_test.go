package outletpolicy

import "testing"

func strptr(s string) *string { return &s }
func boolptr(b bool) *bool    { return &b }

func TestNormalizeUseCase(t *testing.T) {
	cases := map[string]string{
		"hospitality": UseCaseHospitality,
		"HOTEL":       UseCaseHospitality,
		"Bar":         UseCaseHospitality,
		"cafe":        UseCaseHospitality,
		"restaurant":  UseCaseHospitality,
		"quick_service": UseCaseQuickService,
		"quick service": UseCaseQuickService,
		"pharmacy":    UseCasePharmacy,
		"salon":       UseCaseServices,
		"spa":         UseCaseServices,
		"clinic":      UseCaseServices,
		"services":    UseCaseServices,
		"retail":      UseCaseRetail,
		"":            UseCaseRetail,
		"unknown":     UseCaseRetail,
	}
	for in, want := range cases {
		if got := NormalizeUseCase(in); got != want {
			t.Errorf("NormalizeUseCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveCashierSalesVisibility(t *testing.T) {
	// Defaults: hospitality => own, others => outlet.
	if got := ResolveCashierSalesVisibility("hospitality", nil); got != VisibilityOwn {
		t.Errorf("hospitality default = %q, want own", got)
	}
	for _, uc := range []string{"retail", "quick_service", "pharmacy", "services"} {
		if got := ResolveCashierSalesVisibility(uc, nil); got != VisibilityOutlet {
			t.Errorf("%s default = %q, want outlet", uc, got)
		}
	}
	// Override wins.
	if got := ResolveCashierSalesVisibility("hospitality", strptr("outlet")); got != VisibilityOutlet {
		t.Errorf("override outlet = %q, want outlet", got)
	}
	// Unrecognized override falls back to default.
	if got := ResolveCashierSalesVisibility("retail", strptr("bogus")); got != VisibilityOutlet {
		t.Errorf("bogus override = %q, want outlet (default)", got)
	}
}

func TestResolveAutoLogoutAfterSale(t *testing.T) {
	// Defaults: hospitality + quick_service => true, others => false.
	for _, uc := range []string{"hospitality", "quick_service", "hotel", "bar"} {
		if !ResolveAutoLogoutAfterSale(uc, nil) {
			t.Errorf("%s default auto-logout = false, want true", uc)
		}
	}
	for _, uc := range []string{"retail", "pharmacy", "services"} {
		if ResolveAutoLogoutAfterSale(uc, nil) {
			t.Errorf("%s default auto-logout = true, want false", uc)
		}
	}
	// Override wins both ways.
	if ResolveAutoLogoutAfterSale("hospitality", boolptr(false)) {
		t.Error("override false ignored")
	}
	if !ResolveAutoLogoutAfterSale("retail", boolptr(true)) {
		t.Error("override true ignored")
	}
}

func TestResolveCashierTerminalSurface(t *testing.T) {
	// All use cases default to full_till.
	for _, uc := range []string{"hospitality", "retail", "quick_service", "pharmacy", "services"} {
		if got := ResolveCashierTerminalSurface(uc, nil); got != SurfaceFullTill {
			t.Errorf("%s default surface = %q, want full_till", uc, got)
		}
	}
	if got := ResolveCashierTerminalSurface("hospitality", strptr("bills_only")); got != SurfaceBillsOnly {
		t.Errorf("override bills_only = %q, want bills_only", got)
	}
}
