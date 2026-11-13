package customers

import (
	"testing"

	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"
)

// An id is trusted only when the CRM returned it for this tenant and it holds the phone it was
// sent with (S2-8 review M-8).
func TestTrustGiven(t *testing.T) {
	mine, foreign, other := uuid.New(), uuid.New(), uuid.New()
	found := map[uuid.UUID]crm.Contact{
		mine:  {ID: mine, Phone: "+254712000111"},
		other: {ID: other, Phone: "+254722000222"},
	}
	got := trustGiven(found, []GivenContact{
		{ID: mine, Phone: "0712000111"},    // same number, another spelling
		{ID: foreign, Phone: "0712000111"}, // not returned: another tenant's or forged
		{ID: other, Phone: "0733000333"},   // this tenant's contact, but another person
	})
	if !got[mine] || got[foreign] || got[other] {
		t.Errorf("trusted = %v", got)
	}
	if trustGiven(found, []GivenContact{{ID: mine}})[mine] {
		t.Error("an id sent without a phone must not be trusted")
	}
	// The same id sent once with a matching and once with another phone is not trusted.
	if trustGiven(found, []GivenContact{{ID: mine, Phone: "0712000111"}, {ID: mine, Phone: "0799000999"}})[mine] {
		t.Error("an id sent for two different people must not be trusted")
	}
}

func TestTrustedContactsWithoutCRMTrustsNothing(t *testing.T) {
	var r *Resolver
	got, err := r.TrustedContacts(t.Context(), uuid.New(), []GivenContact{{ID: uuid.New(), Phone: "0712000111"}})
	if err == nil || len(got) != 0 {
		t.Errorf("got %v, %v; want nothing trusted and an error", got, err)
	}
	if got, err := r.TrustedContacts(t.Context(), uuid.New(), nil); err != nil || len(got) != 0 {
		t.Errorf("no ids: got %v, %v", got, err)
	}
}
