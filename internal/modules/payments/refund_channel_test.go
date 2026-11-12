package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

// A refund goes back the way the customer paid, so treasury books it against the same account
// the sale went to (M-Pesa, MTN and Airtel to the mobile money float, cash and Other to cash).
func TestRefundChannelForMethod(t *testing.T) {
	cases := map[string]string{
		"cash":        "cash",
		"CASH":        "cash",
		"room_charge": "room_folio",
		// An M-Pesa code payment refunds as a code payment, so a tenant whose Till account
		// defaults to mpesa_manual gets the refund on that same account; STK and C2B stay mpesa.
		"mpesa_manual":        "mpesa_manual",
		"manual":              "mpesa_manual",
		"mpesa":               "mpesa",
		"mpesa_stk":           "mpesa",
		"mpesa_c2b":           "mpesa",
		"mtn_momo_manual":     "mtn_momo_manual",
		"airtel_money_manual": "airtel_money_manual",
		"other_manual":        "other_manual",
		"card_manual":         "card_manual",
		"pdq":                 "card_manual",
		"card_terminal":       "card_manual",
		"card":                "card_manual",
		"bank_transfer":       "bank",
		"cheque":              "cheque",
		"":                    "cash",
		"something_new":       "cash",
	}
	for method, want := range cases {
		if got := RefundChannelForMethod(method); got != want {
			t.Errorf("RefundChannelForMethod(%q) = %q, want %q", method, got, want)
		}
	}
}

func seedPaidOrder(t *testing.T, client *ent.Client, tenantID uuid.UUID, pays ...struct {
	method string
	amount float64
	status string
}) *ent.POSOrder {
	t.Helper()
	ctx := context.Background()
	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(100).SetTaxTotal(0).SetTotalAmount(100).SetPaidTotal(100).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	for _, p := range pays {
		if _, err := client.POSPayment.Create().
			SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(p.amount).SetCurrency("KES").
			SetStatus(p.status).SetPaymentData(map[string]any{"method": p.method}).
			Save(ctx); err != nil {
			t.Fatalf("seed payment: %v", err)
		}
	}
	return order
}

type paid = struct {
	method string
	amount float64
	status string
}

// The sale's original method is the money tender that carried the most. Voided payments,
// credit and other non-money tenders are not ways money came in, so they are skipped.
func TestOriginalRefundChannel(t *testing.T) {
	_, client := newTestPaymentsService(t)
	tid := uuid.New()
	ctx := context.Background()

	mtn := seedPaidOrder(t, client, tid, paid{"mtn_momo_manual", 100, StatusCompleted})
	if got := OriginalRefundChannel(ctx, client, tid, mtn.ID); got != "mtn_momo_manual" {
		t.Errorf("MTN sale refunds via %q, want mtn_momo_manual", got)
	}

	split := seedPaidOrder(t, client, tid,
		paid{"cash", 30, StatusCompleted},
		paid{"airtel_money_manual", 70, StatusCompleted},
		paid{"mpesa_manual", 500, StatusVoided},
		paid{TenderCustomerCredit, 200, StatusCompleted},
	)
	if got := OriginalRefundChannel(ctx, client, tid, split.ID); got != "airtel_money_manual" {
		t.Errorf("split sale refunds via %q, want airtel_money_manual (the largest money tender)", got)
	}

	other := seedPaidOrder(t, client, tid, paid{"other_manual", 100, StatusCompleted})
	if got := OriginalRefundChannel(ctx, client, tid, other.ID); got != "other_manual" {
		t.Errorf("Other sale refunds via %q, want other_manual", got)
	}

	legacy := seedPaidOrder(t, client, tid, paid{"manual", 100, StatusCompleted})
	if got := OriginalRefundChannel(ctx, client, tid, legacy.ID); got != "mpesa_manual" {
		t.Errorf("legacy manual sale refunds via %q, want mpesa_manual", got)
	}

	// A row with no payment_data.method falls back to its tender's type.
	noMethod := seedPaidOrder(t, client, tid)
	tdr := client.Tender.Create().SetTenantID(tid).SetName("Airtel").SetType("airtel_money_manual").SaveX(ctx)
	client.POSPayment.Create().
		SetOrderID(noMethod.ID).SetTenderID(tdr.ID).SetAmount(100).SetCurrency("KES").SetStatus(StatusCompleted).
		SaveX(ctx)
	if got := OriginalRefundChannel(ctx, client, tid, noMethod.ID); got != "airtel_money_manual" {
		t.Errorf("tender-type fallback refunds via %q, want airtel_money_manual", got)
	}

	none := seedPaidOrder(t, client, tid)
	if got := OriginalRefundChannel(ctx, client, tid, none.ID); got != "" {
		t.Errorf("a sale with no money payment gives %q, want empty (the caller keeps its own default)", got)
	}

	if got := OriginalRefundChannel(ctx, client, uuid.New(), mtn.ID); got != "" {
		t.Errorf("another tenant's order gives %q, want empty", got)
	}
}
