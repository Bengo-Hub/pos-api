package events

import "testing"

// TestAffectsAvailability pins the contract with inventory-api's auto_hide_on_stock_out policy:
// only an explicit false makes a stock event alert-only; legacy events keep their meaning.
func TestAffectsAvailability(t *testing.T) {
	if !affectsAvailability(map[string]interface{}{"sku": "X"}) {
		t.Error("legacy event without the field must still toggle availability")
	}
	if !affectsAvailability(map[string]interface{}{"affects_availability": true}) {
		t.Error("opted-in tenant must toggle availability")
	}
	if affectsAvailability(map[string]interface{}{"affects_availability": false}) {
		t.Error("manual-only tenant must not toggle availability")
	}
}
