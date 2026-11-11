package handlers

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/appointment"
	"github.com/bengobox/pos-service/internal/ent/clientrecord"
	"github.com/bengobox/pos-service/internal/ent/customerbalancecache"
	"github.com/bengobox/pos-service/internal/ent/eventbooking"
	"github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	"github.com/bengobox/pos-service/internal/ent/roombooking"
	"github.com/bengobox/pos-service/internal/ent/roomguest"
	"github.com/bengobox/pos-service/internal/ent/tenant"
)

// CRMContactReferenced reports whether any POS table, in any tenant, still carries the CRM contact
// id. CRM contact ids are random UUIDs minted by marketflow, so a match anywhere is this person,
// and a tenant mismatch (rows held under a drifted tenant id) can only answer "referenced", which
// keeps the contact. Merged loyalty shells count too (a shell keeps the id until it is cleared).
func CRMContactReferenced(ctx context.Context, db *ent.Client, contactID uuid.UUID) (bool, error) {
	checks := []func() (bool, error){
		func() (bool, error) {
			return db.LoyaltyAccount.Query().Where(loyaltyaccount.CrmContactID(contactID)).Exist(ctx)
		},
		func() (bool, error) {
			return db.ClientRecord.Query().Where(clientrecord.CrmContactID(contactID)).Exist(ctx)
		},
		func() (bool, error) {
			return db.CustomerBalanceCache.Query().Where(customerbalancecache.CrmContactID(contactID)).Exist(ctx)
		},
		func() (bool, error) {
			return db.Appointment.Query().Where(appointment.CrmContactID(contactID)).Exist(ctx)
		},
		func() (bool, error) {
			return db.RoomBooking.Query().Where(roombooking.CrmContactID(contactID)).Exist(ctx)
		},
		func() (bool, error) {
			return db.RoomGuest.Query().Where(roomguest.CrmContactID(contactID)).Exist(ctx)
		},
		func() (bool, error) {
			return db.EventBooking.Query().Where(eventbooking.CrmContactID(contactID)).Exist(ctx)
		},
	}
	for _, check := range checks {
		found, err := check()
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

// S2SCRMLinks tells marketflow whether POS still links a CRM contact, so marketflow can decide
// between deleting an erased person's contact and stripping it to a minimal record. {tenant} is
// the tenant UUID or slug and must be known here; the contact is matched in every tenant. The answer is the flag only: no names, phones or record ids.
// GET /api/v1/s2s/{tenant}/crm-links/{contactID} (X-API-Key) -> {"referenced": bool}
func (h *LoyaltyHandler) S2SCRMLinks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	contactID, err := uuid.Parse(chi.URLParam(r, "contactID"))
	if err != nil || contactID == uuid.Nil {
		jsonErrorWithCode(w, "contact id must be a UUID", "invalid_contact_id", http.StatusBadRequest)
		return
	}
	// The tenant must be known here, by UUID or slug. An unknown UUID (POS rows held under another
	// tenant id after a sync drift) answers 404, never "not referenced": marketflow then keeps the
	// contact instead of deleting it.
	raw := chi.URLParam(r, "tenant")
	pred := tenant.Slug(raw)
	if id, perr := uuid.Parse(raw); perr == nil {
		pred = tenant.ID(id)
	}
	t, err := h.db.Tenant.Query().Where(pred).Only(ctx)
	if ent.IsNotFound(err) {
		jsonErrorWithCode(w, "tenant not found", "tenant_not_found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.log.Error("crm links: tenant lookup failed", zap.Error(err))
		jsonError(w, "failed to check links", http.StatusInternalServerError)
		return
	}
	tenantID := t.ID
	found, err := CRMContactReferenced(ctx, h.db, contactID)
	if err != nil {
		// Ids only in the log, never the person's details.
		h.log.Error("crm links check failed", zap.String("tenant_id", tenantID.String()),
			zap.String("contact_id", contactID.String()), zap.Error(err))
		jsonError(w, "failed to check links", http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusOK, map[string]bool{"referenced": found})
}
