package payments

import (
	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/platform/events"
)

// saleBuyer is the buyer pos.sale.finalized names: the resolved CRM contact when there is one,
// else the name and phone captured at the till. An unparsable or empty contact id counts as
// unlinked, so the phone fallback is never lost.
func saleBuyer(crmContactID, name, phone string) events.Recipient {
	if id, err := uuid.Parse(crmContactID); err == nil && id != uuid.Nil {
		return events.Recipient{CrmContactID: &id}
	}
	return events.Recipient{Name: name, Phone: phone}
}
