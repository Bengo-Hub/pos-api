package customers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

func TestDeleteAccountWithShellsRemovesEmptyShells(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	shell, survivor := mergedPair(t, db, tid)
	db.LoyaltyAccount.UpdateOneID(survivor.ID).SetPointsBalance(0).ExecX(ctx)
	kept := db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("254712345678").SetCustomerName("Odd Shell").
		SetPointsBalance(5).SetMetadata(map[string]any{MergedIntoKey: survivor.ID.String()}).
		SaveX(ctx)
	// A shell of another tenant pointing at the same id is never touched.
	otherTenant := uuid.New()
	foreign := db.LoyaltyAccount.Create().
		SetTenantID(otherTenant).SetCustomerPhone("0712345678").SetCustomerName("Other Tenant").
		SetMetadata(map[string]any{MergedIntoKey: survivor.ID.String()}).
		SaveX(ctx)

	res, err := DeleteAccountWithShells(ctx, db, tid, survivor.ID)
	if err != nil {
		t.Fatalf("DeleteAccountWithShells: %v", err)
	}
	if !res.Deleted || res.ShellsDeleted != 1 || len(res.ShellsKept) != 1 || res.ShellsKept[0] != kept.ID {
		t.Fatalf("result = %+v, want deleted with one shell removed and the odd one kept", res)
	}
	for _, id := range []uuid.UUID{survivor.ID, shell.ID} {
		if _, err := db.LoyaltyAccount.Get(ctx, id); !ent.IsNotFound(err) {
			t.Errorf("account %s still exists (err=%v)", id, err)
		}
	}
	for _, id := range []uuid.UUID{kept.ID, foreign.ID} {
		if _, err := db.LoyaltyAccount.Get(ctx, id); err != nil {
			t.Errorf("account %s should be kept: %v", id, err)
		}
	}
}

func TestDeleteAccountWithShellsKeepsAccountWithPoints(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	shell, survivor := mergedPair(t, db, tid) // the survivor holds 130 points

	res, err := DeleteAccountWithShells(ctx, db, tid, survivor.ID)
	if err != nil || res.Deleted {
		t.Fatalf("result = %+v, %v; want nothing deleted while the account holds points", res, err)
	}
	for _, id := range []uuid.UUID{survivor.ID, shell.ID} {
		if _, err := db.LoyaltyAccount.Get(ctx, id); err != nil {
			t.Errorf("account %s should be kept: %v", id, err)
		}
	}
}

func TestFindAccountForCreateReleasesOrphanShell(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	// The survivor was deleted by a path that did not take its shells along.
	shell, survivor := mergedPair(t, db, tid)
	db.LoyaltyAccount.DeleteOneID(survivor.ID).ExecX(ctx)

	if _, err := FindAccountIn(ctx, db, tid, "+254712345678"); !errors.Is(err, ErrMergeChainBroken) {
		t.Fatalf("FindAccountIn = %v, want ErrMergeChainBroken before release", err)
	}
	// Earn on the explicit shell id still refuses.
	if _, err := EarnPoints(ctx, db, tid, shell.ID, 5, nil, ""); !errors.Is(err, ErrMergeChainBroken) {
		t.Fatalf("EarnPoints on an orphan shell = %v, want ErrMergeChainBroken", err)
	}

	got, err := FindAccountForCreate(ctx, db, tid, "+254712345678", nil)
	if err != nil || got == nil || got.ID != shell.ID {
		t.Fatalf("FindAccountForCreate = %v, %v; want the released shell %s", got, err, shell.ID)
	}
	if IsMerged(got.Metadata) {
		t.Error("released shell is still marked merged")
	}
	orphan, _ := got.Metadata[OrphanedMergeKey].(map[string]any)
	if orphan[MergedIntoKey] != survivor.ID.String() {
		t.Errorf("released shell metadata = %v, want the old marker kept under %s", got.Metadata, OrphanedMergeKey)
	}
	// It is an ordinary account again: lookups and earns work.
	if again, err := FindAccountIn(ctx, db, tid, "0712345678"); err != nil || again == nil || again.ID != shell.ID {
		t.Errorf("FindAccountIn after release = %v, %v", again, err)
	}
	if res, err := EarnPoints(ctx, db, tid, shell.ID, 5, nil, ""); err != nil || res.Account.PointsBalance != 5 {
		t.Errorf("EarnPoints after release = %+v, %v", res, err)
	}
}

func TestFindAccountForCreateOrphanWithPointsGivesNone(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("0712345678").SetCustomerName("Odd Orphan").
		SetPointsBalance(5).SetCreatedAt(mergeT0.Add(-time.Hour)).
		SetMetadata(map[string]any{MergedIntoKey: uuid.NewString()}).
		SaveX(ctx)

	got, err := FindAccountForCreate(ctx, db, tid, "+254712345678", nil)
	if err != nil || got != nil {
		t.Fatalf("FindAccountForCreate = %v, %v; want no live account (and no error) so a new one can be made", got, err)
	}
}

func TestEmptyMergedMarkerIsLiveInGoAndSQL(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	acc := db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("0712345678").SetCustomerName("Blank Marker").
		SetMetadata(map[string]any{MergedIntoKey: ""}).
		SaveX(ctx)
	shell := db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("0722000111").SetCustomerName("Real Shell").
		SetMetadata(map[string]any{MergedIntoKey: acc.ID.String()}).
		SaveX(ctx)

	if IsMerged(acc.Metadata) {
		t.Error(`IsMerged with merged_into "" = true, want false`)
	}
	ids, err := db.LoyaltyAccount.Query().Where(AccountNotMerged()).IDs(ctx)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(ids) != 1 || ids[0] != acc.ID {
		t.Errorf("AccountNotMerged = %v, want only %s (shell %s excluded)", ids, acc.ID, shell.ID)
	}
}
