package identity

import "testing"

// TestMapSSORoleToPOS pins which SSO roles become POS staff with PIN login. technician (the
// services production board) and barista were dropped before, so a demo technician landed in
// POS as a cashier.
func TestMapSSORoleToPOS(t *testing.T) {
	cases := map[string]string{
		"admin":        "admin",
		"superuser":    "admin",
		"manager":      "manager",
		"staff":        "cashier",
		"cashier":      "cashier",
		"technician":   "technician",
		"barista":      "barista",
		"stylist":      "stylist",
		"therapist":    "therapist",
		"receptionist": "receptionist",
		"viewer":       "",
		"rider":        "",
		"customer":     "",
	}
	for in, want := range cases {
		got := mapSSORoleToPOS(map[string]interface{}{"roles": []interface{}{in}})
		if got != want {
			t.Errorf("mapSSORoleToPOS(%q) = %q, want %q", in, got, want)
		}
	}
}
