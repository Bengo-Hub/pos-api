package handlers

import "testing"

func TestPaymentMethodLabel_ManualTenders(t *testing.T) {
	cases := []struct {
		code string
		data map[string]any
		want string
	}{
		{"mtn_momo_manual", nil, "MTN MoMo (Code)"},
		{"airtel_money_manual", nil, "Airtel Money (Code)"},
		{"mpesa_manual", nil, "M-Pesa (Code)"},
		{"other_manual", map[string]any{"payment_label": " Equity cheque "}, "Other (Equity cheque)"},
		{"other_manual", map[string]any{}, "Other"},
		{"other_manual", nil, "Other"},
		{"cash", map[string]any{"payment_label": "ignored"}, "Cash"},
	}
	for _, c := range cases {
		if got := paymentMethodLabel(c.code, c.data); got != c.want {
			t.Errorf("paymentMethodLabel(%q, %v) = %q, want %q", c.code, c.data, got, c.want)
		}
	}
}

// The hotel desk has no format or duplicate check for these codes yet, so it refuses them: they
// are never immediate hotel tenders, which also makes check-in refuse them.
func TestHotelRefusesTillOnlyManualTenders(t *testing.T) {
	for _, m := range []string{"mtn_momo_manual", "airtel_money_manual", "other_manual", " MTN_MOMO_MANUAL "} {
		if isImmediateHotelMethod(m) {
			t.Errorf("isImmediateHotelMethod(%q) = true, want false", m)
		}
		if !isTillOnlyManualTender(m) {
			t.Errorf("isTillOnlyManualTender(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"cash", "mpesa", "mpesa_manual", "card_manual"} {
		if isTillOnlyManualTender(m) {
			t.Errorf("isTillOnlyManualTender(%q) = true, want false", m)
		}
	}
}
