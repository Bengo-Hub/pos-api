package payments

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The hotel desk applies the till's rules: MTN and Airtel IDs in the transaction ID shape,
// an M-Pesa code that is present, Other with a 2 to 40 character label. Codes are uppercased.
func TestNormalizeManualCode(t *testing.T) {
	type want struct{ method, ref, label string }
	ok := []struct {
		method, ref, label string
		want               want
	}{
		{"mpesa_manual", " qb234abcde ", "x", want{"mpesa_manual", "QB234ABCDE", ""}},
		{"manual", "qb234abcde", "", want{"mpesa_manual", "QB234ABCDE", ""}},
		{"MTN_MOMO_MANUAL", " 12345678901 ", "", want{"mtn_momo_manual", "12345678901", ""}},
		{"airtel_money_manual", "ci251111.1530.a12345", "", want{"airtel_money_manual", "CI251111.1530.A12345", ""}},
		{"other_manual", " chq 001 ", " Equity cheque ", want{"other_manual", "chq 001", "Equity cheque"}},
		{"cash", " anything ", "label", want{"cash", " anything ", ""}},
	}
	for _, c := range ok {
		m, ref, label, err := NormalizeManualCode(c.method, c.ref, c.label)
		if err != nil {
			t.Errorf("%s %q: unexpected error %v", c.method, c.ref, err)
			continue
		}
		if (want{m, ref, label}) != c.want {
			t.Errorf("%s %q: got (%q, %q, %q), want %+v", c.method, c.ref, m, ref, label, c.want)
		}
	}

	bad := []struct{ method, ref, label string }{
		{"mpesa_manual", "  ", ""},
		{"mtn_momo_manual", "ABC12", ""},
		{"airtel_money_manual", "", ""},
		{"other_manual", "", "A"},
		{"other_manual", strings.Repeat("R", 41), "Voucher"},
	}
	for _, c := range bad {
		if _, _, _, err := NormalizeManualCode(c.method, c.ref, c.label); err == nil {
			t.Errorf("%s %q label %q: want a validation error", c.method, c.ref, c.label)
		} else if strings.HasPrefix(err.Error(), "payments:") {
			t.Errorf("%s: the message is shown at the desk and must not carry the package prefix: %q", c.method, err)
		}
	}
}

// A code that settled a till sale or a hotel folio cannot settle another one. Each network's
// codes are checked only against that network, like the till.
func TestManualCodeUsed(t *testing.T) {
	_, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()

	order := seedOrderForMpesaCodeTest(t, client, tid)
	if _, err := client.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(100).SetCurrency("KES").
		SetStatus(StatusCompleted).SetExternalReference("MTN1234567").
		SetPaymentData(map[string]any{"method": "mtn_momo_manual"}).
		Save(ctx); err != nil {
		t.Fatalf("seed till payment: %v", err)
	}
	if _, err := client.RoomFolioPayment.Create().
		SetTenantID(tid).SetRoomID(uuid.New()).SetRoomGuestID(uuid.New()).
		SetAmount(100).SetMethod("mpesa").SetReference("QB234ABCDE").SetStatus("completed").
		Save(ctx); err != nil {
		t.Fatalf("seed folio payment: %v", err)
	}
	if _, err := client.RoomFolioPayment.Create().
		SetTenantID(tid).SetRoomID(uuid.New()).SetRoomGuestID(uuid.New()).
		SetAmount(100).SetMethod("airtel_money_manual").SetReference("AIR1234567").SetStatus("failed").
		Save(ctx); err != nil {
		t.Fatalf("seed failed folio payment: %v", err)
	}

	cases := []struct {
		name         string
		tenant       uuid.UUID
		method, code string
		want         bool
	}{
		{"till MTN code reused at the desk", tid, "mtn_momo_manual", "mtn1234567", true},
		{"same digits on another network", tid, "airtel_money_manual", "MTN1234567", false},
		{"folio M-Pesa code (stored as mpesa) reused", tid, "mpesa_manual", "QB234ABCDE", true},
		{"failed folio payment does not count", tid, "airtel_money_manual", "AIR1234567", false},
		{"another tenant", uuid.New(), "mtn_momo_manual", "MTN1234567", false},
		{"fresh code", tid, "mpesa_manual", "NEWCODE123", false},
		{"other_manual is never checked", tid, "other_manual", "QB234ABCDE", false},
	}
	for _, c := range cases {
		got, err := ManualCodeUsed(ctx, client, c.tenant, c.method, c.code)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: ManualCodeUsed = %v, want %v", c.name, got, c.want)
		}
	}
}
