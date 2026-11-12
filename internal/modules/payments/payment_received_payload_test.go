package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent/pospayment"
)

// pos.payment.received_notification names a CRM-linked customer by crm_contact_id only; an
// unlinked customer keeps customer_name and customer_phone so the notice still reaches them.
func TestPaymentReceivedPayloadIdsOnlyForLinkedCustomer(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tid := uuid.New()

	linked := seedPaidOrder(t, client, tid, paid{"mpesa_manual", 100, StatusCompleted})
	client.POSOrder.UpdateOneID(linked.ID).SetCustomerName("Jane Wanjiku").SetCustomerPhone("0712345678").ExecX(ctx)
	contact := uuid.New()
	client.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("+254712345678").SetCustomerName("Jane Wanjiku").SetCrmContactID(contact).
		SaveX(ctx)
	order := client.POSOrder.GetX(ctx, linked.ID)
	p := client.POSPayment.Query().Where(pospayment.OrderID(linked.ID)).OnlyX(ctx)

	got := svc.paymentReceivedPayload(ctx, tid, order, p, "mpesa_manual")
	if got["crm_contact_id"] != contact.String() {
		t.Errorf("crm_contact_id = %v, want %s", got["crm_contact_id"], contact)
	}
	for _, k := range []string{"customer_name", "customer_phone", "customer_email"} {
		if _, ok := got[k]; ok {
			t.Errorf("linked customer payload carries %q", k)
		}
	}
	if got["payment_id"] != p.ID.String() || got["order_number"] != order.OrderNumber || got["amount"] != 100.0 || got["method"] != "mpesa_manual" {
		t.Errorf("payment fields missing: %v", got)
	}

	walkIn := seedPaidOrder(t, client, tid, paid{"cash", 50, StatusCompleted})
	client.POSOrder.UpdateOneID(walkIn.ID).SetCustomerName("Sam Otieno").SetCustomerPhone("0799000111").ExecX(ctx)
	wo := client.POSOrder.GetX(ctx, walkIn.ID)
	walkPay := client.POSPayment.Query().Where(pospayment.OrderID(walkIn.ID)).OnlyX(ctx)
	got2 := svc.paymentReceivedPayload(ctx, tid, wo, walkPay, "cash")
	if got2["customer_name"] != "Sam Otieno" || got2["customer_phone"] != "0799000111" {
		t.Errorf("unlinked customer lost the fallback fields: %v", got2)
	}
	if _, ok := got2["crm_contact_id"]; ok {
		t.Error("unlinked customer has no crm_contact_id")
	}
}
