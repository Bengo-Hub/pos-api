package orders

import "testing"

func TestPaymentStatusMatches(t *testing.T) {
	for _, c := range []struct {
		filter, status string
		want           bool
	}{
		{"", "paid", true},
		{"due", "due", true},
		{"due", "partial", false},
		{"owed", "due", true},
		{"owed", "partial", true},
		{"owed", "overdue", true},
		{"owed", "paid", false},
		{"owed", "refunded", false},
	} {
		if got := PaymentStatusMatches(c.filter, c.status); got != c.want {
			t.Errorf("PaymentStatusMatches(%q, %q) = %v, want %v", c.filter, c.status, got, c.want)
		}
	}
}
