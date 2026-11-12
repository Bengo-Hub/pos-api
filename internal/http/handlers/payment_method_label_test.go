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

// The hotel desk now takes MTN MoMo, Airtel Money and Other codes like the till: they settle
// immediately and treasury records each as itself.
func TestHotelTakesManualCodeTenders(t *testing.T) {
	for _, m := range []string{"mtn_momo_manual", "airtel_money_manual", "other_manual", " MTN_MOMO_MANUAL "} {
		if !isImmediateHotelMethod(m) {
			t.Errorf("isImmediateHotelMethod(%q) = false, want true", m)
		}
	}
	for in, want := range map[string]string{
		"mtn_momo_manual": "mtn_momo_manual", "airtel_money_manual": "airtel_money_manual",
		"other_manual": "other_manual", "mpesa": "mpesa_manual",
	} {
		if got := treasuryMethodForHotel(in); got != want {
			t.Errorf("treasuryMethodForHotel(%q) = %q, want %q", in, got, want)
		}
	}
}
