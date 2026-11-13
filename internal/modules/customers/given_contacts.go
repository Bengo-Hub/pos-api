package customers

import (
	"context"
	"strings"

	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"
)

// GivenContact is a crm_contact_id a caller sent with the phone it was sent for.
type GivenContact struct {
	ID    uuid.UUID
	Phone string
}

// batchGetLimit is the most ids marketflow's internal batch read takes in one call.
const batchGetLimit = 200

// TrustedContacts checks crm_contact_ids sent by a client (the POS client form, a bulk import)
// against the CRM, the same rule the hotel desk follows (S2-8 review M-8): an id is kept only
// when the contact belongs to this tenant and holds the phone it was sent with. A foreign or
// forged id would otherwise make the crm-links check answer "referenced" for another tenant's
// contact, so its erasure keeps data instead of deleting it.
//
// It returns the set of ids that may be stored. The CRM is read in batches of 200 within the
// link budget. When the CRM cannot be asked (not configured, down) nothing is trusted and the
// error is returned; callers then save the record unlinked and the reconciler links it by phone.
func (r *Resolver) TrustedContacts(ctx context.Context, tenantID uuid.UUID, given []GivenContact) (map[uuid.UUID]bool, error) {
	trusted := make(map[uuid.UUID]bool, len(given))
	ids := make([]uuid.UUID, 0, len(given))
	seen := make(map[uuid.UUID]bool, len(given))
	for _, g := range given {
		if g.ID != uuid.Nil && !seen[g.ID] {
			seen[g.ID] = true
			ids = append(ids, g.ID)
		}
	}
	if len(ids) == 0 {
		return trusted, nil
	}
	if r == nil || r.crm == nil {
		return trusted, crm.ErrDisabled
	}
	found := make(map[uuid.UUID]crm.Contact, len(ids))
	for start := 0; start < len(ids); start += batchGetLimit {
		end := min(start+batchGetLimit, len(ids))
		callCtx, cancel := context.WithTimeout(ctx, linkBudget)
		got, err := r.crm.BatchGet(callCtx, tenantID, ids[start:end])
		cancel()
		if err != nil {
			return map[uuid.UUID]bool{}, err
		}
		for id, c := range got {
			found[id] = c
		}
	}
	return trustGiven(found, given), nil
}

// trustGiven keeps an id when the CRM returned it and every phone it was sent with matches the
// contact (any spelling of the number). An id sent without a phone is never trusted.
func trustGiven(found map[uuid.UUID]crm.Contact, given []GivenContact) map[uuid.UUID]bool {
	trusted := make(map[uuid.UUID]bool, len(given))
	bad := make(map[uuid.UUID]bool)
	for _, g := range given {
		c, ok := found[g.ID]
		if !ok || strings.TrimSpace(g.Phone) == "" || !ContactMatchesGuest(c, g.Phone, "") {
			bad[g.ID] = true
			continue
		}
		trusted[g.ID] = true
	}
	for id := range bad {
		delete(trusted, id)
	}
	return trusted
}
