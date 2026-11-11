package customers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/Bengo-Hub/httpware/contact"
	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	"github.com/bengobox/pos-service/internal/ent/predicate"
)

// mergedIntoAny matches shells whose merged_into is one of ids.
func mergedIntoAny(ids []uuid.UUID) predicate.LoyaltyAccount {
	return predicate.LoyaltyAccount(func(s *sql.Selector) {
		col := s.C(entla.FieldMetadata)
		preds := make([]*sql.Predicate, 0, len(ids))
		for _, id := range ids {
			preds = append(preds, sqljson.ValueEQ(col, id.String(), sqljson.Path(MergedIntoKey)))
		}
		s.Where(sql.Or(preds...))
	})
}

// DeleteResult is what DeleteAccountWithShells did.
type DeleteResult struct {
	// Deleted is false when the account gained points since it was read; nothing was deleted.
	Deleted bool
	// ShellsDeleted counts the empty merged shells removed with it.
	ShellsDeleted int
	// ShellsKept lists shells that pointed at the account but hold points, so were left alone.
	// By construction a shell holds none; one that does needs a person to look at it.
	ShellsKept []uuid.UUID
}

// DeleteAccountWithShells deletes a zero-point loyalty account together with the empty shells
// merged into it (and shells merged into those, up to maxMergeHops), in one transaction.
//
// Without this, deleting a survivor leaves shells whose merged_into points at a missing row,
// and every phone lookup for that number fails (see ErrMergeChainBroken). The account is only
// deleted while it still holds no points; a shell holding points is never deleted.
func DeleteAccountWithShells(ctx context.Context, db *ent.Client, tenantID, accountID uuid.UUID) (DeleteResult, error) {
	var res DeleteResult
	tx, err := db.Tx(ctx)
	if err != nil {
		return res, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var empty []uuid.UUID
	targets := []uuid.UUID{accountID}
	for hop := 0; hop < maxMergeHops && len(targets) > 0; hop++ {
		shells, err := tx.LoyaltyAccount.Query().
			Where(entla.TenantID(tenantID), mergedIntoAny(targets)).
			All(ctx)
		if err != nil {
			return res, fmt.Errorf("load merged shells: %w", err)
		}
		targets = targets[:0:0]
		for _, s := range shells {
			if s.PointsBalance != 0 {
				res.ShellsKept = append(res.ShellsKept, s.ID)
				continue
			}
			empty = append(empty, s.ID)
			targets = append(targets, s.ID)
		}
	}
	if len(empty) > 0 {
		n, err := tx.LoyaltyAccount.Delete().
			Where(entla.TenantID(tenantID), entla.IDIn(empty...), entla.PointsBalance(0)).
			Exec(ctx)
		if err != nil {
			return res, fmt.Errorf("delete merged shells: %w", err)
		}
		res.ShellsDeleted = n
	}
	n, err := tx.LoyaltyAccount.Delete().
		Where(entla.ID(accountID), entla.TenantID(tenantID), entla.PointsBalanceLTE(0)).
		Exec(ctx)
	if err != nil {
		return res, fmt.Errorf("delete loyalty account: %w", err)
	}
	if n == 0 {
		// Points arrived since the caller read it: keep everything.
		return DeleteResult{}, nil
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit: %w", err)
	}
	committed = true
	res.Deleted = true
	return res, nil
}

// FindAccountForCreate is FindAccountForContact for the paths that create an account when none
// is found (staff registration and the S2S find-or-create). When the number only matches shells
// whose survivor is gone (ErrMergeChainBroken), those orphaned shells are released: their merge
// marker is moved to metadata.orphaned_merge and they become ordinary, empty accounts again, so
// the lookup returns one of them instead of failing. Creating a fresh account beside them would
// usually hit the unique (tenant, phone) index anyway, because a shell keeps its phone.
//
// Earn and redeem on an explicit shell id still refuse through LockAccount.
func FindAccountForCreate(ctx context.Context, db *ent.Client, tenantID uuid.UUID, phone string, contactID *uuid.UUID) (*ent.LoyaltyAccount, error) {
	acc, err := FindAccountForContact(ctx, db, tenantID, phone, contactID)
	if !errors.Is(err, ErrMergeChainBroken) {
		return acc, err
	}
	if err := releaseOrphanShells(ctx, db, tenantID, phone); err != nil {
		return nil, err
	}
	acc, err = FindAccountForContact(ctx, db, tenantID, phone, contactID)
	if errors.Is(err, ErrMergeChainBroken) {
		// A shell that could not be released (it holds points) still matches: report no live
		// account rather than block the customer.
		return nil, nil
	}
	return acc, err
}

// OrphanedMergeKey holds, on a released shell, the merge marker it carried.
const OrphanedMergeKey = "orphaned_merge"

// releaseOrphanShells turns the tenant's shells for this number whose survivor cannot be reached
// back into ordinary empty accounts. Shells holding points are left alone.
func releaseOrphanShells(ctx context.Context, db *ent.Client, tenantID uuid.UUID, phone string) error {
	raw := strings.TrimSpace(phone)
	norm, spellings, ok := lookupSpellings(raw)
	q := db.LoyaltyAccount.Query().Where(entla.TenantID(tenantID))
	if ok {
		q = q.Where(entla.Or(entla.CustomerPhoneIn(spellings...),
			entla.CustomerPhoneHasSuffix(contact.SubscriberDigits(norm))))
	} else {
		q = q.Where(entla.CustomerPhoneIn(spellings...))
	}
	rows, err := q.All(ctx)
	if err != nil {
		return fmt.Errorf("load shells: %w", err)
	}
	for _, s := range rows {
		if !IsMerged(s.Metadata) || s.PointsBalance != 0 {
			continue
		}
		if ok && NormalisePhone(s.CustomerPhone) != norm {
			continue
		}
		if _, err := followMerged(ctx, db, tenantID, []*ent.LoyaltyAccount{s}); !errors.Is(err, ErrMergeChainBroken) {
			if err != nil {
				return err
			}
			continue // its survivor is reachable
		}
		meta := make(map[string]any, len(s.Metadata))
		orphan := map[string]any{}
		for k, v := range s.Metadata {
			switch k {
			case MergedIntoKey, MergedAtKey, MergedReasonKey:
				orphan[k] = v
			default:
				meta[k] = v
			}
		}
		meta[OrphanedMergeKey] = orphan
		if _, err := db.LoyaltyAccount.Update().
			Where(entla.ID(s.ID), entla.PointsBalance(0)).
			SetMetadata(meta).
			Save(ctx); err != nil {
			return fmt.Errorf("release orphaned shell: %w", err)
		}
	}
	return nil
}
