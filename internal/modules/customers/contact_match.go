package customers

import (
	"context"
	"strings"

	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"
)

// ContactMatchesGuest reports whether a CRM contact is the person a front desk typed in: the
// contact holds the same phone (any spelling of the number) or the same email (ignoring case).
// Empty values never match.
func ContactMatchesGuest(c crm.Contact, phone, email string) bool {
	if strings.TrimSpace(phone) != "" && SamePhone(c.Phone, phone) {
		return true
	}
	e := strings.TrimSpace(email)
	return e != "" && strings.EqualFold(strings.TrimSpace(c.Email), e)
}

// ContactMatches loads a contact from the CRM within the link budget and reports whether it is
// the guest with this phone or email. A contact the CRM does not know is not a match. An error
// means the CRM could not be asked (not configured, down, or refusing the key), so the caller
// cannot trust the id either way.
func (r *Resolver) ContactMatches(ctx context.Context, tenantID, contactID uuid.UUID, phone, email string) (bool, error) {
	if r == nil || r.crm == nil {
		return false, crm.ErrDisabled
	}
	callCtx, cancel := context.WithTimeout(ctx, linkBudget)
	defer cancel()
	got, err := r.crm.BatchGet(callCtx, tenantID, []uuid.UUID{contactID})
	if err != nil {
		return false, err
	}
	c, ok := got[contactID]
	if !ok {
		return false, nil
	}
	return ContactMatchesGuest(c, phone, email), nil
}
