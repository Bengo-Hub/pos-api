package orders

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent/enttest"
)

// A receipt for a CRM-linked customer names them by crm_contact_id alone: the name and the
// phone stored on the sale stay in pos-api, and notifications-api reads them from the CRM.
func TestSaleNoticeRecipientLinkedSendsOnlyTheContactID(t *testing.T) {
	id := uuid.New()
	p := addSaleNoticeRecipient(map[string]any{"order_id": "o1"}, &id, "Jane", "+254712345678", "", "", "")
	if p["crm_contact_id"] != id.String() || p["order_id"] != "o1" {
		t.Fatalf("got %v, want the contact id and the order id", p)
	}
	for _, k := range []string{"customer_name", "customer_phone", "customer_email"} {
		if _, ok := p[k]; ok {
			t.Errorf("linked payload must not carry %q", k)
		}
	}
}

// A phone or email the cashier typed for this one send is where the receipt goes, so it stays on
// a linked customer's event; the name never does.
func TestSaleNoticeRecipientLinkedKeepsTypedDestination(t *testing.T) {
	id := uuid.New()
	p := addSaleNoticeRecipient(map[string]any{}, &id, "Jane", "+254712345678", "jane@example.com", "0799000111", "jane@example.com")
	if p["crm_contact_id"] != id.String() {
		t.Fatalf("got %v, want the contact id", p)
	}
	if p["customer_phone"] != "0799000111" || p["customer_email"] != "jane@example.com" {
		t.Fatalf("typed destination lost: %v", p)
	}
	if _, ok := p["customer_name"]; ok {
		t.Error("linked payload must not carry customer_name")
	}
}

// A customer with no CRM link keeps name, phone and email so the receipt still reaches them.
func TestSaleNoticeRecipientUnlinkedKeepsFields(t *testing.T) {
	p := addSaleNoticeRecipient(map[string]any{}, nil, "Ann", "0711111111", "ann@example.com", "", "ann@example.com")
	if p["customer_name"] != "Ann" || p["customer_phone"] != "0711111111" || p["customer_email"] != "ann@example.com" {
		t.Fatalf("got %v, want the customer fields", p)
	}
	if _, ok := p["crm_contact_id"]; ok {
		t.Error("unlinked payload has no crm_contact_id")
	}
}

// The sale's customer is found through the loyalty account on the sale's phone, in the sale's own
// tenant only.
func TestSaleCustomerContactID(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:salecontact_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	tid, other := uuid.New(), uuid.New()
	contactID := uuid.New()
	client.LoyaltyAccount.Create().SetTenantID(tid).SetCustomerPhone("+254712345678").SetCustomerName("Jane").
		SetCrmContactID(contactID).SaveX(ctx)
	client.LoyaltyAccount.Create().SetTenantID(other).SetCustomerPhone("+254700000001").SetCustomerName("X").
		SetCrmContactID(uuid.New()).SaveX(ctx)
	svc := &Service{client: client}

	order := client.POSOrder.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").SetSubtotal(1).SetTaxTotal(0).
		SetTotalAmount(1).SetPaidTotal(1).SetCustomerPhone("0712345678").SaveX(ctx)
	if got := svc.saleCustomerContactID(ctx, tid, order); got == nil || *got != contactID {
		t.Fatalf("linked customer: got %v, want %s", got, contactID)
	}

	// Another tenant's account on the same number is never used.
	foreign := client.POSOrder.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").SetSubtotal(1).SetTaxTotal(0).
		SetTotalAmount(1).SetPaidTotal(1).SetCustomerPhone("+254700000001").SaveX(ctx)
	if got := svc.saleCustomerContactID(ctx, tid, foreign); got != nil {
		t.Fatalf("another tenant's contact used: %v", got)
	}

	// No phone on the sale: unlinked.
	bare := client.POSOrder.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").SetSubtotal(1).SetTaxTotal(0).
		SetTotalAmount(1).SetPaidTotal(1).SaveX(ctx)
	if got := svc.saleCustomerContactID(ctx, tid, bare); got != nil {
		t.Fatalf("no phone: got %v", got)
	}
}
