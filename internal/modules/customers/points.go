package customers

import (
	"context"
	"errors"
	"fmt"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	"github.com/bengobox/pos-service/internal/ent/predicate"
)

// maxMergeHops bounds how many merged shells a lookup or lock follows before giving up.
const maxMergeHops = 4

// ErrMergeChainBroken means a merged shell points to an account that is missing, or the chain
// of shells is longer than maxMergeHops. Callers must stop rather than create a new account.
var ErrMergeChainBroken = errors.New("merged loyalty account points to a missing account")

// MergedError is returned by LockAccount when the locked row is a merged shell and the caller
// asked not to follow it. SurvivorID is the account the shell was merged into.
type MergedError struct {
	AccountID  uuid.UUID
	SurvivorID uuid.UUID
}

func (e *MergedError) Error() string {
	return "loyalty account " + e.AccountID.String() + " was merged into " + e.SurvivorID.String()
}

// lockRow locks the selected rows FOR UPDATE on Postgres. SQLite has no row locks (a write
// transaction holds the whole database), so the clause is left out there.
func lockRow() predicate.LoyaltyAccount {
	return predicate.LoyaltyAccount(func(s *sql.Selector) {
		if s.Dialect() == dialect.Postgres {
			s.ForUpdate()
		}
	})
}

// LockAccount locks a tenant's loyalty account inside tx and returns it. When the row is a
// merged shell, follow decides: true locks and returns the account it was merged into (shell
// first, then survivor, the same order the merge writes them, so the two never deadlock);
// false returns a *MergedError so the caller can tell its client to look the customer up
// again.
//
// A call that was waiting on the row lock while a merge committed sees the merged row once it
// gets the lock (Postgres re-reads a locked row after the other transaction commits), so it is
// redirected or refused like any other call.
func LockAccount(ctx context.Context, tx *ent.Tx, tenantID, accountID uuid.UUID, follow bool) (*ent.LoyaltyAccount, error) {
	id := accountID
	for hop := 0; hop <= maxMergeHops; hop++ {
		acc, err := tx.LoyaltyAccount.Query().
			Where(entla.ID(id), entla.TenantID(tenantID), lockRow()).
			Only(ctx)
		if err != nil {
			if hop > 0 && ent.IsNotFound(err) {
				return nil, ErrMergeChainBroken
			}
			return nil, err
		}
		if !IsMerged(acc.Metadata) {
			return acc, nil
		}
		next, ok := mergedInto(acc.Metadata)
		if !ok {
			return nil, ErrMergeChainBroken
		}
		if !follow {
			return nil, &MergedError{AccountID: acc.ID, SurvivorID: next}
		}
		id = next
	}
	return nil, ErrMergeChainBroken
}

// EarnResult is the outcome of EarnPoints.
type EarnResult struct {
	// Account is the credited account after the update. It is the survivor when the id given
	// was a merged shell.
	Account *ent.LoyaltyAccount
	// PreviousLifetime is the lifetime points before this credit, for tier checks.
	PreviousLifetime int
	Transaction      *ent.LoyaltyTransaction
}

// EarnPoints credits points to a loyalty account in one transaction: it locks the row
// (following a merged shell to its survivor), adds the points and lifetime points in SQL
// rather than writing back a value read earlier, and records an "earn" transaction with the
// balance after. orderID and notes are optional.
func EarnPoints(ctx context.Context, db *ent.Client, tenantID, accountID uuid.UUID, points int, orderID *uuid.UUID, notes string) (EarnResult, error) {
	return CreditPoints(ctx, db, tenantID, accountID, points, "earn", orderID, notes)
}

// CreditPoints is EarnPoints with the transaction type given, for credits such as a referral
// bonus ("referral").
func CreditPoints(ctx context.Context, db *ent.Client, tenantID, accountID uuid.UUID, points int, kind string, orderID *uuid.UUID, notes string) (EarnResult, error) {
	if points <= 0 {
		return EarnResult{}, errors.New("points must be positive")
	}
	tx, err := db.Tx(ctx)
	if err != nil {
		return EarnResult{}, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	acc, err := LockAccount(ctx, tx, tenantID, accountID, true)
	if err != nil {
		return EarnResult{}, err
	}
	updated, err := tx.LoyaltyAccount.UpdateOneID(acc.ID).
		AddPointsBalance(points).
		AddLifetimePoints(points).
		Save(ctx)
	if err != nil {
		return EarnResult{}, fmt.Errorf("credit points: %w", err)
	}
	create := tx.LoyaltyTransaction.Create().
		SetTenantID(tenantID).
		SetAccountID(acc.ID).
		SetTypeField(kind).
		SetPoints(points).
		SetBalanceAfter(updated.PointsBalance).
		SetNillableOrderID(orderID)
	if notes != "" {
		create = create.SetNotes(notes)
	}
	ltx, err := create.Save(ctx)
	if err != nil {
		return EarnResult{}, fmt.Errorf("record earn transaction: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return EarnResult{}, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return EarnResult{Account: updated, PreviousLifetime: acc.LifetimePoints, Transaction: ltx}, nil
}
