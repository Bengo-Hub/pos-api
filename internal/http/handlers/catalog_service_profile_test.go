package handlers

import (
	"testing"

	"github.com/bengobox/pos-service/internal/modules/outletpolicy"
)

func TestServiceProfileAllowsItem(t *testing.T) {
	printing, _ := outletpolicy.LookupServiceProfile(outletpolicy.ProfilePrintingBranding)
	laundry, _ := outletpolicy.LookupServiceProfile(outletpolicy.ProfileLaundryDryCleaning)

	cases := []struct {
		name       string
		profile    *outletpolicy.ServiceProfile
		itemType   string
		useCase    string
		category   string
		categoryOK bool
		want       bool
	}{
		{"no profile keeps the category gate (allow)", nil, "SERVICE", "SALON_SERVICE", "Salon", true, true},
		{"no profile keeps the category gate (deny)", nil, "GOODS", "RETAIL", "Stationery", false, false},
		{"printing service item", &printing, "SERVICE", "PRINTING_SERVICE", "Printing", true, true},
		{"generic professional service", &printing, "SERVICE", "PROFESSIONAL_SERVICE", "Design", true, true},
		{"legacy untagged service", &printing, "SERVICE", "RETAIL", "Printing", true, true},
		{"salon service hidden at a print shop", &printing, "SERVICE", "SALON_SERVICE", "Salon", true, false},
		{"stationery goods at a print shop", &printing, "GOODS", "RETAIL", "Stationery", false, true},
		{"food never sold at a print shop", &printing, "GOODS", "RETAIL", "Hot Beverages", false, false},
		{"components never sold", &printing, "GOODS", "RETAIL", "Raw Materials", false, false},
		{"laundry does not sell merchandise", &laundry, "GOODS", "RETAIL", "Stationery", false, false},
	}
	for _, c := range cases {
		if got := serviceProfileAllowsItem(c.profile, c.itemType, c.useCase, c.category, c.categoryOK); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBookableServiceUseCases(t *testing.T) {
	for _, uc := range []string{"SALON_SERVICE", "NAIL_SERVICE", "SPA_SERVICE"} {
		if !isBookableServiceUseCase(uc) {
			t.Errorf("%s must be bookable", uc)
		}
	}
	for _, uc := range []string{"PRINTING_SERVICE", "LAUNDRY_SERVICE", "RETAIL"} {
		if isBookableServiceUseCase(uc) {
			t.Errorf("%s must not be bookable", uc)
		}
	}
}
