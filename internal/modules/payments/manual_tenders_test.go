package payments

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeManualTender_MobileMoneyCodeFormat(t *testing.T) {
	for _, method := range []string{TenderMTNMoMoManual, TenderAirtelMoneyManual} {
		ok := []struct{ in, want string }{
			{"  ab12cd34  ", "AB12CD34"},
			{"CI251111.1530.A12345", "CI251111.1530.A12345"},
			{"mp-123456", "MP-123456"},
			{"123456", "123456"},
		}
		for _, c := range ok {
			req := RecordPaymentRequest{TenderMethod: method, ExternalRef: c.in, PaymentLabel: "ignored"}
			if err := normalizeManualTender(&req); err != nil {
				t.Errorf("%s %q: unexpected error %v", method, c.in, err)
				continue
			}
			if req.ExternalRef != c.want {
				t.Errorf("%s %q: ref = %q, want %q", method, c.in, req.ExternalRef, c.want)
			}
			if req.PaymentLabel != "" {
				t.Errorf("%s: label should be dropped, got %q", method, req.PaymentLabel)
			}
		}
		bad := []string{"", "   ", "ABC12", "-ABC123", ".ABC123", "ABC 123", "ABC_123", strings.Repeat("A", 31)}
		for _, in := range bad {
			req := RecordPaymentRequest{TenderMethod: method, ExternalRef: in}
			if err := normalizeManualTender(&req); err == nil {
				t.Errorf("%s %q: want a validation error", method, in)
			}
		}
	}
}

func TestNormalizeManualTender_OtherNeedsLabel(t *testing.T) {
	req := RecordPaymentRequest{TenderMethod: TenderOtherManual, PaymentLabel: "  Equity cheque ", ExternalRef: " chq 001 "}
	if err := normalizeManualTender(&req); err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if req.PaymentLabel != "Equity cheque" || req.ExternalRef != "chq 001" {
		t.Errorf("got label %q ref %q, want trimmed values", req.PaymentLabel, req.ExternalRef)
	}

	noRef := RecordPaymentRequest{TenderMethod: TenderOtherManual, PaymentLabel: "Voucher"}
	if err := normalizeManualTender(&noRef); err != nil {
		t.Errorf("reference is optional for other_manual, got %v", err)
	}

	for _, label := range []string{"", " ", "A", strings.Repeat("x", 41)} {
		r := RecordPaymentRequest{TenderMethod: TenderOtherManual, PaymentLabel: label}
		if err := normalizeManualTender(&r); err == nil {
			t.Errorf("label %q: want a validation error", label)
		}
	}

	long := RecordPaymentRequest{TenderMethod: TenderOtherManual, PaymentLabel: "Voucher", ExternalRef: strings.Repeat("R", 41)}
	if err := normalizeManualTender(&long); err == nil {
		t.Errorf("reference over 40 characters: want a validation error")
	}
}

func TestNormalizeManualTender_LeavesOtherTendersAlone(t *testing.T) {
	req := RecordPaymentRequest{TenderMethod: "mpesa_manual", ExternalRef: " uhmeq4bfaa ", PaymentLabel: "x"}
	if err := normalizeManualTender(&req); err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if req.ExternalRef != " uhmeq4bfaa " {
		t.Errorf("mpesa_manual ref changed to %q", req.ExternalRef)
	}
	if req.PaymentLabel != "" {
		t.Errorf("label should only survive on other_manual, got %q", req.PaymentLabel)
	}
}

func TestManualTenders_SettleImmediatelyAsThemselves(t *testing.T) {
	for _, m := range []string{TenderMTNMoMoManual, TenderAirtelMoneyManual, TenderOtherManual} {
		if !isCashMethod(m) {
			t.Errorf("isCashMethod(%q) = false, want true", m)
		}
		if got := treasuryMethodForImmediate(m); got != m {
			t.Errorf("treasuryMethodForImmediate(%q) = %q, want %q", m, got, m)
		}
	}
	if got := treasuryMethodForImmediate("manual"); got != "mpesa_manual" {
		t.Errorf("legacy manual maps to %q, want mpesa_manual", got)
	}
}

func TestIsManualCodeReused_ScopedByTenantAndMethod(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	order := seedOrderForMpesaCodeTest(t, client, tenantID)

	if _, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(100).SetCurrency("UGX").
		SetStatus(StatusCompleted).SetExternalReference("MP251111ABCD").
		SetPaymentData(map[string]any{"method": TenderMTNMoMoManual}).
		Save(ctx); err != nil {
		t.Fatalf("seed MTN payment: %v", err)
	}

	check := func(name string, tenant uuid.UUID, method, code string, want bool) {
		t.Helper()
		got, err := svc.isManualCodeReused(ctx, tenant, method, code)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: reused = %v, want %v", name, got, want)
		}
	}
	check("same MTN code again", tenantID, TenderMTNMoMoManual, "MP251111ABCD", true)
	check("same MTN code lowercased", tenantID, TenderMTNMoMoManual, "mp251111abcd", true)
	check("same code as Airtel", tenantID, TenderAirtelMoneyManual, "MP251111ABCD", false)
	check("same code as M-Pesa", tenantID, "mpesa_manual", "MP251111ABCD", false)
	check("same code in another tenant", uuid.New(), TenderMTNMoMoManual, "MP251111ABCD", false)
	check("fresh MTN code", tenantID, TenderMTNMoMoManual, "MP251111WXYZ", false)
}

func TestWithPaymentLabel(t *testing.T) {
	data := withPaymentLabel(cashPaymentData(TenderOtherManual, 100, 0), "Voucher")
	if data["payment_label"] != "Voucher" || data["method"] != TenderOtherManual {
		t.Errorf("got %v, want method and payment_label", data)
	}
	plain := withPaymentLabel(cashPaymentData("cash", 100, 0), "")
	if _, ok := plain["payment_label"]; ok {
		t.Errorf("empty label must not be stored, got %v", plain)
	}
}
