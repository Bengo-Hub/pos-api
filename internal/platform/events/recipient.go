package events

import (
	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

// Recipient is the person a customer notice event is about.
type Recipient struct {
	CrmContactID *uuid.UUID
	Name         string
	Email        string
	Phone        string
}

// WithRecipient adds to an event payload who its notice goes to, for notifications-api.
//
// A person linked to the CRM is named by crm_contact_id alone: notifications-api fetches the
// name, email and phone from the CRM when it sends, so no personal data travels on the bus.
// A person with no CRM link (the CRM was down or refused the contact, or the record predates
// linking) keeps <prefix>_name, <prefix>_email and <prefix>_phone in the payload, which is how
// their notices still reach them. That fallback stays until every record is linked.
//
// crm_contact_id always names the person the notice goes to (for a referral bonus, the
// referrer). The payload is changed in place and returned.
func WithRecipient(payload map[string]any, prefix string, r Recipient) map[string]any {
	if r.CrmContactID != nil && *r.CrmContactID != uuid.Nil {
		payload["crm_contact_id"] = r.CrmContactID.String()
		return payload
	}
	payload[prefix+"_name"] = r.Name
	payload[prefix+"_email"] = r.Email
	payload[prefix+"_phone"] = r.Phone
	return payload
}

// LoyaltyAccountRecipient is the customer who owns a loyalty account.
func LoyaltyAccountRecipient(acc *ent.LoyaltyAccount) Recipient {
	return Recipient{
		CrmContactID: acc.CrmContactID,
		Name:         acc.CustomerName,
		Email:        acc.CustomerEmail,
		Phone:        acc.CustomerPhone,
	}
}

// LoyaltyPointsEarnedPayload is the pos.loyalty.points.earned payload for points credited to
// acc. The caller adds order_id when the points came from a sale.
func LoyaltyPointsEarnedPayload(acc *ent.LoyaltyAccount, points, balanceAfter int) map[string]any {
	return WithRecipient(map[string]any{
		"account_id":    acc.ID.String(),
		"points_earned": points,
		"balance_after": balanceAfter,
	}, "customer", LoyaltyAccountRecipient(acc))
}
