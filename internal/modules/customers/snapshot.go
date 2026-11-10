package customers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Bengo-Hub/httpware/contact"
	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entappt "github.com/bengobox/pos-service/internal/ent/appointment"
	entclient "github.com/bengobox/pos-service/internal/ent/clientrecord"
	enteb "github.com/bengobox/pos-service/internal/ent/eventbooking"
	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	entrb "github.com/bengobox/pos-service/internal/ent/roombooking"
	entrg "github.com/bengobox/pos-service/internal/ent/roomguest"
)

// RefreshSnapshots loads the given contacts from the CRM and rewrites the snapshot fields of
// every local record linked to them. Ids the CRM no longer knows are skipped.
func (r *Resolver) RefreshSnapshots(ctx context.Context, tenantID uuid.UUID, ids []uuid.UUID) error {
	if r == nil || r.crm == nil || len(ids) == 0 {
		return nil
	}
	contacts, err := r.crm.BatchGet(ctx, tenantID, ids)
	if err != nil {
		if errors.Is(err, crm.ErrDisabled) {
			return nil
		}
		return fmt.Errorf("load contacts: %w", err)
	}
	for _, c := range contacts {
		if err := r.ApplySnapshot(ctx, tenantID, c); err != nil {
			return err
		}
	}
	return nil
}

// ApplySnapshot writes the CRM contact's name, phone and email onto the loyalty accounts and
// client records linked to it. Hotel guests and bookings are never rewritten. A phone that
// would collide with another record's unique phone is not written; the old phone stays and a
// warning is logged.
func (r *Resolver) ApplySnapshot(ctx context.Context, tenantID uuid.UUID, c crm.Contact) error {
	if r == nil || r.db == nil || c.ID == uuid.Nil {
		return nil
	}
	ident := mergeIdentity(Identity{}, c)
	ident.Email = strings.TrimSpace(c.Email) // the CRM's empty email is the truth here

	accounts, err := r.db.LoyaltyAccount.Query().
		Where(entla.TenantID(tenantID), entla.CrmContactID(c.ID)).
		All(ctx)
	if err != nil {
		return fmt.Errorf("load loyalty accounts: %w", err)
	}
	for _, acc := range accounts {
		if err := r.snapshotAccount(ctx, acc, ident); err != nil {
			return err
		}
	}

	clients, err := r.db.ClientRecord.Query().
		Where(entclient.TenantID(tenantID), entclient.CrmContactID(c.ID)).
		All(ctx)
	if err != nil {
		return fmt.Errorf("load client records: %w", err)
	}
	for _, rec := range clients {
		if err := r.snapshotClient(ctx, rec, ident); err != nil {
			return err
		}
	}
	// Room guests, room bookings and event bookings keep only the link. Their names, phones and
	// emails are the guest register as staff recorded it, so a contact shared by an agent, a
	// company mailbox or a spouse never rewrites it.
	return nil
}

// snapshotAccount writes ident onto one loyalty account. Points are never touched.
func (r *Resolver) snapshotAccount(ctx context.Context, acc *ent.LoyaltyAccount, ident Identity) error {
	phone := ""
	if ident.Phone != "" && ident.Phone != acc.CustomerPhone {
		// Any other account holding the same number, in any spelling, is a collision.
		cands, err := r.db.LoyaltyAccount.Query().
			Where(entla.TenantID(acc.TenantID), entla.IDNEQ(acc.ID), entla.Or(
				entla.CustomerPhone(ident.Phone),
				entla.CustomerPhoneHasSuffix(contact.SubscriberDigits(ident.Phone)),
			)).
			Limit(suffixCandidates).
			All(ctx)
		if err != nil {
			return fmt.Errorf("check phone collision: %w", err)
		}
		var otherIDs []string
		for _, c := range cands {
			if c.CustomerPhone == ident.Phone || SamePhone(c.CustomerPhone, ident.Phone) {
				otherIDs = append(otherIDs, c.ID.String())
			}
		}
		if len(otherIDs) > 0 {
			r.log.Warn("loyalty account phone kept: the CRM phone belongs to another account",
				zap.String("account_id", acc.ID.String()), zap.Strings("other_account_ids", otherIDs))
		} else {
			phone = ident.Phone
		}
	}

	build := func(withPhone bool) (*ent.LoyaltyAccountUpdateOne, bool) {
		upd := r.db.LoyaltyAccount.UpdateOneID(acc.ID)
		changed := false
		if ident.CrmContactID != nil && (acc.CrmContactID == nil || *acc.CrmContactID != *ident.CrmContactID) {
			upd = upd.SetCrmContactID(*ident.CrmContactID)
			changed = true
		}
		if ident.Name != "" && ident.Name != acc.CustomerName {
			upd = upd.SetCustomerName(ident.Name)
			changed = true
		}
		if ident.Email != acc.CustomerEmail {
			upd = upd.SetCustomerEmail(ident.Email)
			changed = true
		}
		if withPhone && phone != "" {
			upd = upd.SetCustomerPhone(phone)
			changed = true
		}
		return upd, changed
	}

	upd, changed := build(true)
	if !changed {
		return nil
	}
	err := upd.Exec(ctx)
	if err != nil && phone != "" && ent.IsConstraintError(err) {
		// Another account took the phone between the check and the write.
		r.log.Warn("loyalty account phone kept: a concurrent write took the CRM phone",
			zap.String("account_id", acc.ID.String()))
		upd, changed = build(false)
		if !changed {
			return nil
		}
		err = upd.Exec(ctx)
	}
	if err != nil {
		return fmt.Errorf("update loyalty account: %w", err)
	}
	return nil
}

// snapshotClient writes the CRM link and phone onto one client record.
func (r *Resolver) snapshotClient(ctx context.Context, rec *ent.ClientRecord, ident Identity) error {
	phone := ""
	if ident.Phone != "" && ident.Phone != rec.Phone {
		cands, err := r.db.ClientRecord.Query().
			Where(entclient.TenantID(rec.TenantID), entclient.IDNEQ(rec.ID), entclient.Or(
				entclient.Phone(ident.Phone),
				entclient.PhoneHasSuffix(contact.SubscriberDigits(ident.Phone)),
			)).
			Limit(suffixCandidates).
			All(ctx)
		if err != nil {
			return fmt.Errorf("check phone collision: %w", err)
		}
		var otherIDs []string
		for _, c := range cands {
			if c.Phone == ident.Phone || SamePhone(c.Phone, ident.Phone) {
				otherIDs = append(otherIDs, c.ID.String())
			}
		}
		if len(otherIDs) > 0 {
			r.log.Warn("client record phone kept: the CRM phone belongs to another record",
				zap.String("client_id", rec.ID.String()), zap.Strings("other_client_ids", otherIDs))
		} else {
			phone = ident.Phone
		}
	}

	build := func(withPhone bool) (*ent.ClientRecordUpdateOne, bool) {
		upd := r.db.ClientRecord.UpdateOneID(rec.ID)
		changed := false
		if ident.CrmContactID != nil && (rec.CrmContactID == nil || *rec.CrmContactID != *ident.CrmContactID) {
			upd = upd.SetCrmContactID(*ident.CrmContactID)
			changed = true
		}
		if withPhone && phone != "" {
			upd = upd.SetPhone(phone)
			changed = true
		}
		return upd, changed
	}

	upd, changed := build(true)
	if !changed {
		return nil
	}
	err := upd.Exec(ctx)
	if err != nil && phone != "" && ent.IsConstraintError(err) {
		r.log.Warn("client record phone kept: a concurrent write took the CRM phone",
			zap.String("client_id", rec.ID.String()))
		upd, changed = build(false)
		if !changed {
			return nil
		}
		err = upd.Exec(ctx)
	}
	if err != nil {
		return fmt.Errorf("update client record: %w", err)
	}
	return nil
}

// Unlink clears a deleted CRM contact's id from every local record that carries it. Loyalty
// accounts and client records also get the crm_unlinked_at marker so the reconciler never
// re-creates the contact. Nothing else changes: points, notes and stays are kept.
func (r *Resolver) Unlink(ctx context.Context, tenantID, contactID uuid.UUID) error {
	if r == nil || r.db == nil || contactID == uuid.Nil {
		return nil
	}
	now := time.Now()
	if err := r.unlinkAccounts(ctx, tenantID, contactID, now); err != nil {
		return err
	}
	if err := r.unlinkClients(ctx, tenantID, contactID, now); err != nil {
		return err
	}
	if _, err := r.db.RoomGuest.Update().
		Where(entrg.TenantID(tenantID), entrg.CrmContactID(contactID)).
		ClearCrmContactID().Save(ctx); err != nil {
		return fmt.Errorf("unlink room guests: %w", err)
	}
	if _, err := r.db.RoomBooking.Update().
		Where(entrb.TenantID(tenantID), entrb.CrmContactID(contactID)).
		ClearCrmContactID().Save(ctx); err != nil {
		return fmt.Errorf("unlink room bookings: %w", err)
	}
	if _, err := r.db.EventBooking.Update().
		Where(enteb.TenantID(tenantID), enteb.CrmContactID(contactID)).
		ClearCrmContactID().Save(ctx); err != nil {
		return fmt.Errorf("unlink event bookings: %w", err)
	}
	if _, err := r.db.Appointment.Update().
		Where(entappt.TenantID(tenantID), entappt.CrmContactID(contactID)).
		ClearCrmContactID().Save(ctx); err != nil {
		return fmt.Errorf("unlink appointments: %w", err)
	}
	return nil
}

// Edit changes a customer's name, phone or email. The change goes to the CRM first and the
// local snapshot is then written from the CRM's answer, so pos never edits identity on its own.
func (r *Resolver) Edit(ctx context.Context, tenantID, contactID uuid.UUID, in crm.UpdateInput) (Identity, error) {
	if r == nil || r.crm == nil {
		return Identity{}, crm.ErrDisabled
	}
	got, err := r.crm.Update(ctx, tenantID, contactID, in)
	if err != nil {
		return Identity{}, err
	}
	if err := r.ApplySnapshot(ctx, tenantID, got); err != nil {
		return Identity{}, err
	}
	return mergeIdentity(Identity{}, got), nil
}
