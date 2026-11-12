package orders

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/customers"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// saleCustomerContactID is the CRM contact of the sale's customer: the one linked to the loyalty
// account on the sale's phone, in the sale's tenant. nil when the sale has no phone, no account
// holds it, or the account has no CRM link; the receipt then carries the customer's details.
func (s *Service) saleCustomerContactID(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder) *uuid.UUID {
	phone := strings.TrimSpace(derefStr(order.CustomerPhone))
	if phone == "" {
		return nil
	}
	acc, err := customers.FindAccountIn(ctx, s.client, tenantID, phone)
	if err != nil || acc == nil || acc.CrmContactID == nil || *acc.CrmContactID == uuid.Nil {
		return nil
	}
	id := *acc.CrmContactID
	return &id
}

// addSaleNoticeRecipient adds who a pos.sale.notification_requested receipt goes to.
//
// A customer linked to the CRM is named by crm_contact_id alone and notifications-api reads the
// name, phone and email from the CRM. The only exception is a phone or email the cashier typed
// for this one send (typedPhone, typedEmail): that is where the receipt must go, so it travels
// as customer_phone or customer_email and wins over the CRM. The name never travels for a linked
// customer. A customer with no CRM link keeps customer_name, customer_phone and customer_email.
func addSaleNoticeRecipient(payload map[string]any, crmContactID *uuid.UUID, name, phone, email, typedPhone, typedEmail string) map[string]any {
	events.WithRecipient(payload, "customer", events.Recipient{CrmContactID: crmContactID, Name: name, Email: email, Phone: phone})
	if _, linked := payload["crm_contact_id"]; !linked {
		return payload
	}
	if v := strings.TrimSpace(typedPhone); v != "" {
		payload["customer_phone"] = v
	}
	if v := strings.TrimSpace(typedEmail); v != "" {
		payload["customer_email"] = v
	}
	return payload
}
