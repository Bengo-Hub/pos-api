package handlers

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

func TestBuildDeliveryTaskRequestCashOnDelivery(t *testing.T) {
	order := &ent.POSOrder{
		ID: uuid.New(), OutletID: uuid.New(), OrderNumber: "000123",
		TotalAmount: 1250.5, PaidTotal: 250,
		CustomerName: strp("Wanjiku"), CustomerPhone: strp("0722000000"),
		Metadata: map[string]any{"delivery_address": "Ngong Rd", "delivery_notes": "Gate B"},
	}
	req := buildDeliveryTaskRequest(order, "Urban Loft Cafe", nil)
	if req.SourceService != "pos" || req.ExternalReference != order.ID.String() {
		t.Fatalf("task reference = %q/%q", req.SourceService, req.ExternalReference)
	}
	if req.PickupAddress != "Urban Loft Cafe" || req.DropoffAddress != "Ngong Rd" || req.Instructions != "Gate B" {
		t.Fatalf("addresses = %+v", req)
	}
	if got := req.Metadata["cash_on_delivery"]; got != 1000.5 {
		t.Fatalf("cash_on_delivery = %v, want the unpaid 1000.5", got)
	}

	order.PaidTotal = order.TotalAmount
	if _, ok := buildDeliveryTaskRequest(order, "", nil).Metadata["cash_on_delivery"]; ok {
		t.Fatal("a paid order must not ask the rider to collect cash")
	}
}

// logistics-api writes the customer's delivery emails from the task. A CRM-linked customer goes
// by crm_contact_id alone (pos-api's ids-only rule); an unlinked one with an email, the order's
// own before the account's. Name and phone always go: the rider needs them at the door.
func TestBuildDeliveryTaskRequestCustomer(t *testing.T) {
	newOrder := func(meta map[string]any) *ent.POSOrder {
		return &ent.POSOrder{
			ID: uuid.New(), OutletID: uuid.New(), OrderNumber: "000124",
			CustomerName: strp("Wanjiku"), CustomerPhone: strp("0722000000"), Metadata: meta,
		}
	}
	contact := uuid.MustParse("6f1c2d3e-0000-4000-8000-000000000001")

	linked := buildDeliveryTaskRequest(newOrder(map[string]any{"customer_email": "order@example.com"}), "Outlet",
		&ent.LoyaltyAccount{CrmContactID: &contact, CustomerEmail: "acc@example.com"})
	if linked.CRMContactID != contact.String() || linked.CustomerEmail != "" {
		t.Fatalf("linked customer must go by id only: crm=%q email=%q", linked.CRMContactID, linked.CustomerEmail)
	}
	if linked.CustomerName != "Wanjiku" || linked.CustomerPhone != "0722000000" {
		t.Fatalf("name and phone must still go for the rider: %+v", linked)
	}

	orderEmail := buildDeliveryTaskRequest(newOrder(map[string]any{"customer_email": " order@example.com "}), "Outlet",
		&ent.LoyaltyAccount{CustomerEmail: "acc@example.com"})
	if orderEmail.CRMContactID != "" || orderEmail.CustomerEmail != "order@example.com" {
		t.Fatalf("unlinked: the order's email wins: crm=%q email=%q", orderEmail.CRMContactID, orderEmail.CustomerEmail)
	}

	nilID := uuid.Nil
	accEmail := buildDeliveryTaskRequest(newOrder(nil), "Outlet",
		&ent.LoyaltyAccount{CrmContactID: &nilID, CustomerEmail: "acc@example.com"})
	if accEmail.CRMContactID != "" || accEmail.CustomerEmail != "acc@example.com" {
		t.Fatalf("unlinked: falls back to the account's email: crm=%q email=%q", accEmail.CRMContactID, accEmail.CustomerEmail)
	}

	none := buildDeliveryTaskRequest(newOrder(nil), "Outlet", nil)
	if none.CRMContactID != "" || none.CustomerEmail != "" {
		t.Fatalf("no account and no email: nothing sent: crm=%q email=%q", none.CRMContactID, none.CustomerEmail)
	}
}
