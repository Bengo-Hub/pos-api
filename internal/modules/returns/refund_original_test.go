package returns

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
)

func seedPayment(t *testing.T, client *ent.Client, orderID uuid.UUID, method string, amount float64) {
	t.Helper()
	if _, err := client.POSPayment.Create().
		SetOrderID(orderID).SetTenderID(uuid.New()).SetAmount(amount).SetCurrency("KES").
		SetStatus("completed").SetPaymentData(map[string]any{"method": method}).
		Save(context.Background()); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
}

// The new mobile money, Other and card channels are accepted, so a refund can go back the way
// the sale was paid.
func TestRefundChannelPtrAcceptsOriginalMethods(t *testing.T) {
	for _, ch := range []string{"mtn_momo_manual", "airtel_money_manual", "other_manual", "card_manual", "mpesa_manual", "cash", "mpesa"} {
		rc := refundChannelPtr(ch)
		if rc == nil || string(*rc) != ch {
			t.Errorf("refundChannelPtr(%q) = %v, want %q", ch, rc, ch)
			continue
		}
		if err := posreturn.RefundChannelValidator(*rc); err != nil {
			t.Errorf("%q fails the ent validator: %v", ch, err)
		}
	}
	if refundChannelPtr("bitcoin") != nil {
		t.Error("an unknown channel must stay nil")
	}
}

// A refund return with no channel chosen refunds to the sale's original method.
func TestCreateReturnDefaultsToTheOriginalMethod(t *testing.T) {
	cases := map[string]string{
		"mtn_momo_manual":     "mtn_momo_manual",
		"airtel_money_manual": "airtel_money_manual",
		"other_manual":        "other_manual",
		"mpesa_manual":        "mpesa_manual",
		"card_manual":         "card_manual",
		"cash":                "cash",
	}
	for method, want := range cases {
		svc, client := newTestService(t)
		tid, outletID := uuid.New(), uuid.New()
		order := seedOrder(t, client, tid, outletID, "completed")
		seedPayment(t, client, order.ID, method, 500)
		lineID := firstLineID(t, client, order.ID)

		ret, err := svc.CreateReturn(context.Background(), tid, CreateReturnRequest{
			OrderID: order.ID, OutletID: outletID, ReturnType: "refund", Reason: "changed mind",
			Lines:       []LineInput{{OrderLineID: lineID, SKU: "SKU-1", Name: "Sample Item", Quantity: 1, TotalPrice: 250}},
			RequestedBy: uuid.New(),
		})
		if err != nil {
			t.Fatalf("%s: CreateReturn: %v", method, err)
		}
		if ret.RefundChannel == nil || string(*ret.RefundChannel) != want {
			t.Errorf("%s sale: refund_channel = %v, want %q", method, ret.RefundChannel, want)
		}
	}
}

// An explicit choice still wins, and store credit is never replaced by the original method.
func TestCreateReturnKeepsAnExplicitChannel(t *testing.T) {
	svc, client := newTestService(t)
	tid, outletID := uuid.New(), uuid.New()
	order := seedOrder(t, client, tid, outletID, "completed")
	seedPayment(t, client, order.ID, "mtn_momo_manual", 500)
	lineID := firstLineID(t, client, order.ID)
	line := []LineInput{{OrderLineID: lineID, SKU: "SKU-1", Name: "Sample Item", Quantity: 1, TotalPrice: 250}}

	ret, err := svc.CreateReturn(context.Background(), tid, CreateReturnRequest{
		OrderID: order.ID, OutletID: outletID, ReturnType: "refund", Reason: "x", RefundChannel: "cash",
		Lines: line, RequestedBy: uuid.New(),
	})
	if err != nil {
		t.Fatalf("CreateReturn: %v", err)
	}
	if ret.RefundChannel == nil || *ret.RefundChannel != posreturn.RefundChannelCash {
		t.Errorf("explicit cash became %v", ret.RefundChannel)
	}

	sc, err := svc.CreateReturn(context.Background(), tid, CreateReturnRequest{
		OrderID: order.ID, OutletID: outletID, ReturnType: "store_credit", Reason: "x",
		Lines: line, RequestedBy: uuid.New(),
	})
	if err != nil {
		t.Fatalf("CreateReturn store credit: %v", err)
	}
	if sc.RefundChannel != nil {
		t.Errorf("store credit return got channel %v, want none (it settles as store credit)", *sc.RefundChannel)
	}
}

// return.completed names a CRM-linked customer by crm_contact_id only; an unlinked customer
// keeps the name and phone so their notice still reaches them.
func TestCompletedPayloadIdsOnlyForLinkedCustomer(t *testing.T) {
	svc, client := newTestService(t)
	ctx := context.Background()
	tid, outletID := uuid.New(), uuid.New()

	linked := seedOrder(t, client, tid, outletID, "completed")
	client.POSOrder.UpdateOneID(linked.ID).SetCustomerName("Jane Wanjiku").SetCustomerPhone("0712345678").ExecX(ctx)
	contact := uuid.New()
	client.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("+254712345678").SetCustomerName("Jane Wanjiku").SetCrmContactID(contact).
		SaveX(ctx)

	ret := &ent.POSReturn{ID: uuid.New(), TenantID: tid, OrderID: linked.ID, OutletID: outletID,
		ReturnNumber: "RET-1", ReturnType: posreturn.ReturnTypeRefund, RefundAmount: 250, Metadata: map[string]any{}}
	p := svc.completedPayload(ctx, ret, nil, "")
	if p["crm_contact_id"] != contact.String() {
		t.Errorf("crm_contact_id = %v, want %s", p["crm_contact_id"], contact)
	}
	for _, k := range []string{"customer_name", "customer_phone", "customer_email"} {
		if _, ok := p[k]; ok {
			t.Errorf("linked customer payload carries %q", k)
		}
	}
	if p["return_id"] != ret.ID || p["order_number"] == "" || p["refund_amount"] != 250.0 {
		t.Errorf("record ids missing: %v", p)
	}

	walkIn := seedOrder(t, client, tid, outletID, "completed")
	client.POSOrder.UpdateOneID(walkIn.ID).SetCustomerName("Sam Otieno").SetCustomerPhone("0799000111").ExecX(ctx)
	ret2 := &ent.POSReturn{ID: uuid.New(), TenantID: tid, OrderID: walkIn.ID, OutletID: outletID,
		ReturnNumber: "RET-2", ReturnType: posreturn.ReturnTypeRefund, Metadata: map[string]any{}}
	p2 := svc.completedPayload(ctx, ret2, nil, "")
	if p2["customer_name"] != "Sam Otieno" || p2["customer_phone"] != "0799000111" {
		t.Errorf("unlinked customer lost the fallback fields: %v", p2)
	}
	if _, ok := p2["crm_contact_id"]; ok {
		t.Error("unlinked customer has no crm_contact_id")
	}
}
