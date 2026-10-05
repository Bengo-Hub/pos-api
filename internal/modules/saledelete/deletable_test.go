package saledelete

import (
	"testing"

	"github.com/bengobox/pos-service/internal/ent"
)

// TestDeletable: finalized sales are deletable by anyone allowed to Delete; an open sale only by a
// platform owner and only while nothing has been paid on it; drafts and voided sales never.
func TestDeletable(t *testing.T) {
	cases := []struct {
		status  string
		paid    float64
		owner   bool
		allowed bool
	}{
		{"completed", 100, false, true},
		{"open", 0, false, false},
		{"open", 0, true, true},
		{"pending_payment", 0, true, true},
		{"open", 50, true, false},
		{"draft", 0, true, false},
		{"voided", 0, true, false},
	}
	for _, c := range cases {
		err := deletable(&ent.POSOrder{Status: c.status, PaidTotal: c.paid}, Request{AllowUnpaidOpen: c.owner})
		if (err == nil) != c.allowed {
			t.Errorf("status %s paid %.0f owner %v: err = %v, want allowed %v", c.status, c.paid, c.owner, err, c.allowed)
		}
	}
}
