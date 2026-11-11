package customers

import (
	"context"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entclient "github.com/bengobox/pos-service/internal/ent/clientrecord"
	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	"github.com/bengobox/pos-service/internal/ent/predicate"
)

// UnlinkedAtKey is the metadata key set when the record's CRM contact was deleted. The
// reconciler never re-links such a record; only an explicit staff create clears it.
const UnlinkedAtKey = "crm_unlinked_at"

// IsUnlinked reports whether metadata carries the deleted-contact marker.
func IsUnlinked(meta map[string]any) bool {
	_, ok := meta[UnlinkedAtKey]
	return ok
}

// withUnlinked returns a copy of meta with the marker set to now.
func withUnlinked(meta map[string]any, now time.Time) map[string]any {
	out := make(map[string]any, len(meta)+1)
	for k, v := range meta {
		out[k] = v
	}
	out[UnlinkedAtKey] = now.UTC().Format(time.RFC3339)
	return out
}

// WithoutUnlinked returns a copy of meta without the deleted-contact marker.
func WithoutUnlinked(meta map[string]any) map[string]any {
	out := make(map[string]any, len(meta))
	for k, v := range meta {
		if k != UnlinkedAtKey {
			out[k] = v
		}
	}
	return out
}

// notUnlinked matches rows whose metadata is null or has no marker. It runs in SQL.
func notUnlinked(sel *sql.Selector, column string) {
	sel.Where(sql.Or(
		sql.IsNull(sel.C(column)),
		sql.Not(sqljson.HasKey(sel.C(column), sqljson.Path(UnlinkedAtKey))),
	))
}

// accountNotUnlinked also leaves out merged shells (see MergeDuplicateAccounts): a shell is
// emptied and unlinked on purpose and must never be linked to the CRM again.
func accountNotUnlinked() predicate.LoyaltyAccount {
	return predicate.LoyaltyAccount(func(sel *sql.Selector) {
		notUnlinked(sel, entla.FieldMetadata)
		notMerged(sel)
	})
}

// AccountNotMerged matches loyalty accounts that are not merged shells. It runs in SQL.
func AccountNotMerged() predicate.LoyaltyAccount {
	return predicate.LoyaltyAccount(notMerged)
}

// notMerged agrees with IsMerged: a row is a shell only when merged_into is a non-empty string,
// so a missing key and an empty value both count as live.
func notMerged(sel *sql.Selector) {
	col := sel.C(entla.FieldMetadata)
	sel.Where(sql.Or(
		sql.IsNull(col),
		sql.Not(sqljson.HasKey(col, sqljson.Path(MergedIntoKey))),
		sqljson.ValueEQ(col, "", sqljson.Path(MergedIntoKey)),
	))
}

func clientNotUnlinked() predicate.ClientRecord {
	return predicate.ClientRecord(func(sel *sql.Selector) { notUnlinked(sel, entclient.FieldMetadata) })
}

// unlinkAccounts clears the link on the tenant's accounts for a deleted contact and sets the
// marker, keeping every other metadata key. Points are not touched.
func (r *Resolver) unlinkAccounts(ctx context.Context, tenantID, contactID uuid.UUID, now time.Time) error {
	accounts, err := r.db.LoyaltyAccount.Query().
		Where(entla.TenantID(tenantID), entla.CrmContactID(contactID)).
		All(ctx)
	if err != nil {
		return fmt.Errorf("load loyalty accounts: %w", err)
	}
	for _, acc := range accounts {
		if err := UnlinkAccount(ctx, r.db, acc, now); err != nil {
			return err
		}
	}
	return nil
}

// UnlinkAccount clears one account's CRM link and sets the marker, keeping every other metadata
// key. Points are not touched.
func UnlinkAccount(ctx context.Context, db *ent.Client, acc *ent.LoyaltyAccount, now time.Time) error {
	if err := db.LoyaltyAccount.UpdateOneID(acc.ID).
		ClearCrmContactID().
		SetMetadata(withUnlinked(acc.Metadata, now)).
		Exec(ctx); err != nil {
		return fmt.Errorf("unlink loyalty account: %w", err)
	}
	return nil
}

// unlinkClients is unlinkAccounts for client records.
func (r *Resolver) unlinkClients(ctx context.Context, tenantID, contactID uuid.UUID, now time.Time) error {
	clients, err := r.db.ClientRecord.Query().
		Where(entclient.TenantID(tenantID), entclient.CrmContactID(contactID)).
		All(ctx)
	if err != nil {
		return fmt.Errorf("load client records: %w", err)
	}
	for _, rec := range clients {
		if err := r.db.ClientRecord.UpdateOneID(rec.ID).
			ClearCrmContactID().
			SetMetadata(withUnlinked(rec.Metadata, now)).
			Exec(ctx); err != nil {
			return fmt.Errorf("unlink client record: %w", err)
		}
	}
	return nil
}

// RelinkAccount is for staff registering a customer whose account was unlinked because the
// CRM contact was deleted: the person came back and gave their details. It links again with
// the given details, clears the marker and writes the snapshot. A CRM rejection is returned and
// the marker is kept. During an outage the marker is cleared so the reconciler links it later.
func (r *Resolver) RelinkAccount(ctx context.Context, acc *ent.LoyaltyAccount, phone, email, name string) (*ent.LoyaltyAccount, error) {
	if r == nil || r.db == nil || acc == nil {
		return acc, nil
	}
	ident, err := r.Link(ctx, acc.TenantID, phone, email, name)
	if err != nil {
		return acc, err
	}
	if err := r.attachAccount(ctx, acc, ident); err != nil {
		return acc, err
	}
	return r.db.LoyaltyAccount.Get(ctx, acc.ID)
}

// attachAccount clears the marker, then writes the CRM link and snapshot when there is one.
func (r *Resolver) attachAccount(ctx context.Context, acc *ent.LoyaltyAccount, ident Identity) error {
	if IsUnlinked(acc.Metadata) {
		meta := WithoutUnlinked(acc.Metadata)
		if err := r.db.LoyaltyAccount.UpdateOneID(acc.ID).SetMetadata(meta).Exec(ctx); err != nil {
			return fmt.Errorf("clear unlinked marker: %w", err)
		}
		acc.Metadata = meta
	}
	if ident.CrmContactID == nil {
		return nil
	}
	return r.snapshotAccount(ctx, acc, ident)
}

// AttachAccount links an existing account to an identity the caller already resolved with
// Link, clearing the deleted-contact marker. Used by staff create paths.
func (r *Resolver) AttachAccount(ctx context.Context, acc *ent.LoyaltyAccount, ident Identity) (*ent.LoyaltyAccount, error) {
	if r == nil || r.db == nil || acc == nil {
		return acc, nil
	}
	if err := r.attachAccount(ctx, acc, ident); err != nil {
		return acc, err
	}
	return r.db.LoyaltyAccount.Get(ctx, acc.ID)
}

// RelinkClient is RelinkAccount for client records, which carry only a phone.
func (r *Resolver) RelinkClient(ctx context.Context, rec *ent.ClientRecord, phone string) (*ent.ClientRecord, error) {
	if r == nil || r.db == nil || rec == nil {
		return rec, nil
	}
	ident, err := r.Link(ctx, rec.TenantID, phone, "", "")
	if err != nil {
		return rec, err
	}
	if IsUnlinked(rec.Metadata) {
		meta := WithoutUnlinked(rec.Metadata)
		if err := r.db.ClientRecord.UpdateOneID(rec.ID).SetMetadata(meta).Exec(ctx); err != nil {
			return rec, fmt.Errorf("clear unlinked marker: %w", err)
		}
		rec.Metadata = meta
	}
	if ident.CrmContactID != nil {
		if err := r.snapshotClient(ctx, rec, ident); err != nil {
			return rec, err
		}
	}
	return r.db.ClientRecord.Get(ctx, rec.ID)
}
