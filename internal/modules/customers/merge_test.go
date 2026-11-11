package customers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
)

var mergeT0 = time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

// seedMergeAccount stores one loyalty account with its own points and lifetime points.
func seedMergeAccount(t *testing.T, db *ent.Client, tid uuid.UUID, phone string, points, lifetime int, at time.Time, contactID *uuid.UUID) *ent.LoyaltyAccount {
	t.Helper()
	acc, err := db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone(phone).SetCustomerName("Merge Test").
		SetPointsBalance(points).SetLifetimePoints(lifetime).SetCreatedAt(at).
		SetNillableCrmContactID(contactID).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed account %q: %v", phone, err)
	}
	return acc
}

func runMerge(t *testing.T, db *ent.Client, tid *uuid.UUID, apply bool) MergeReport {
	t.Helper()
	rep, err := MergeDuplicateAccounts(context.Background(), db, MergeOptions{TenantID: tid, Apply: apply}, mergeT0.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("MergeDuplicateAccounts(apply=%v): %v", apply, err)
	}
	return rep
}

func TestMergeTwoAccountsBalancesAddUp(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	contactID := uuid.New()

	// The legacy row is linked but has fewer lifetime points, so the busy row survives and
	// adopts the link.
	legacy := seedMergeAccount(t, db, tid, "0712345678", 30, 50, mergeT0, &contactID)
	busy := seedMergeAccount(t, db, tid, "+254712345678", 100, 200, mergeT0.Add(time.Hour), nil)

	rep := runMerge(t, db, &tid, true)
	if rep.DryRun || rep.Merged != 1 || rep.Refused != 0 || len(rep.Groups) != 1 {
		t.Fatalf("report = %+v, want one merged group", rep)
	}
	g := rep.Groups[0]
	if g.SurvivorID != busy.ID || g.Status != MergeStatusMerged || g.Reason != ReasonMostLifetimePoints {
		t.Fatalf("group = %+v, want survivor %s merged for %q", g, busy.ID, ReasonMostLifetimePoints)
	}

	survivor := db.LoyaltyAccount.GetX(ctx, busy.ID)
	if survivor.PointsBalance != 130 || survivor.LifetimePoints != 250 {
		t.Errorf("survivor points = %d/%d, want 130/250", survivor.PointsBalance, survivor.LifetimePoints)
	}
	if survivor.CrmContactID == nil || *survivor.CrmContactID != contactID {
		t.Errorf("survivor did not adopt the loser's contact link: %v", survivor.CrmContactID)
	}
	if from, _ := survivor.Metadata[MergedFromKey].([]any); len(from) != 1 || from[0] != legacy.ID.String() {
		t.Errorf("survivor metadata %s = %v, want [%s]", MergedFromKey, survivor.Metadata[MergedFromKey], legacy.ID)
	}

	shell := db.LoyaltyAccount.GetX(ctx, legacy.ID)
	if shell.PointsBalance != 0 || shell.LifetimePoints != 0 || shell.CrmContactID != nil {
		t.Errorf("shell = %d/%d linked %v, want empty and unlinked", shell.PointsBalance, shell.LifetimePoints, shell.CrmContactID)
	}
	if shell.Metadata[MergedIntoKey] != busy.ID.String() {
		t.Errorf("shell %s = %v, want %s", MergedIntoKey, shell.Metadata[MergedIntoKey], busy.ID)
	}
	if shell.Metadata[MergedAtKey] == nil || shell.Metadata[MergedReasonKey] == nil {
		t.Errorf("shell metadata = %v, want merged_at and merged_reason", shell.Metadata)
	}
	if !IsMerged(shell.Metadata) {
		t.Error("IsMerged(shell) = false")
	}
}

func TestMergeRepointsChildren(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	loser := seedMergeAccount(t, db, tid, "0722000111", 10, 10, mergeT0, nil)
	survivor := seedMergeAccount(t, db, tid, "+254722000111", 90, 90, mergeT0.Add(time.Hour), nil)

	txn := db.LoyaltyTransaction.Create().
		SetTenantID(tid).SetAccountID(loser.ID).SetTypeField("earn").SetPoints(10).SetBalanceAfter(10).
		SaveX(ctx)
	asReferrer := db.Referral.Create().
		SetTenantID(tid).SetReferrerAccountID(loser.ID).SetReferredPhone("+254733000222").SetCode("REF-A").
		SaveX(ctx)
	asReferred := db.Referral.Create().
		SetTenantID(tid).SetReferrerAccountID(uuid.New()).SetReferredPhone("+254722000111").SetCode("REF-B").
		SetReferredAccountID(loser.ID).
		SaveX(ctx)
	plan := db.LayawayPlan.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetLoyaltyAccountID(loser.ID).SetCustomerName("Merge Test").
		SetTotalAmount(decimal.NewFromInt(100)).SetDepositAmount(decimal.NewFromInt(10)).
		SetPaidAmount(decimal.NewFromInt(10)).SetRemainingAmount(decimal.NewFromInt(90)).
		SaveX(ctx)
	order := db.POSOrder.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(100).SetTaxTotal(0).SetTotalAmount(100).
		SaveX(ctx)
	pay := db.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(10).SetStatus("completed").
		SetPaymentData(map[string]any{"loyalty_account_id": loser.ID.String(), "points_redeemed": 10}).
		SaveX(ctx)
	otherPay := db.POSPayment.Create().
		SetOrderID(order.ID).SetTenderID(uuid.New()).SetAmount(5).SetStatus("completed").
		SetPaymentData(map[string]any{"method": "cash"}).
		SaveX(ctx)

	rep := runMerge(t, db, &tid, true)
	if rep.Merged != 1 {
		t.Fatalf("report = %+v, want one merged group", rep)
	}

	if got := db.LoyaltyTransaction.GetX(ctx, txn.ID); got.AccountID != survivor.ID {
		t.Errorf("transaction account = %s, want survivor %s", got.AccountID, survivor.ID)
	}
	if got := db.Referral.GetX(ctx, asReferrer.ID); got.ReferrerAccountID != survivor.ID {
		t.Errorf("referral referrer = %s, want survivor", got.ReferrerAccountID)
	}
	if got := db.Referral.GetX(ctx, asReferred.ID); got.ReferredAccountID == nil || *got.ReferredAccountID != survivor.ID {
		t.Errorf("referral referred = %v, want survivor", got.ReferredAccountID)
	}
	if got := db.LayawayPlan.GetX(ctx, plan.ID); got.LoyaltyAccountID == nil || *got.LoyaltyAccountID != survivor.ID {
		t.Errorf("layaway account = %v, want survivor", got.LoyaltyAccountID)
	}
	gotPay := db.POSPayment.GetX(ctx, pay.ID)
	if gotPay.PaymentData["loyalty_account_id"] != survivor.ID.String() {
		t.Errorf("payment loyalty_account_id = %v, want survivor", gotPay.PaymentData["loyalty_account_id"])
	}
	if gotPay.PaymentData[MergedFromPaymentKey] != loser.ID.String() {
		t.Errorf("payment %s = %v, want the old account id", MergedFromPaymentKey, gotPay.PaymentData[MergedFromPaymentKey])
	}
	if got := db.POSPayment.GetX(ctx, otherPay.ID); len(got.PaymentData) != 1 || got.PaymentData["method"] != "cash" {
		t.Errorf("unrelated payment changed: %v", got.PaymentData)
	}
}

func TestMergeRerunIsNoop(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	a := seedMergeAccount(t, db, tid, "0712345678", 5, 5, mergeT0, nil)
	b := seedMergeAccount(t, db, tid, "254712345678", 7, 7, mergeT0.Add(time.Hour), nil)

	if rep := runMerge(t, db, &tid, true); rep.Merged != 1 {
		t.Fatalf("first run = %+v, want one merge", rep)
	}
	before := map[uuid.UUID]*ent.LoyaltyAccount{a.ID: db.LoyaltyAccount.GetX(ctx, a.ID), b.ID: db.LoyaltyAccount.GetX(ctx, b.ID)}

	for _, apply := range []bool{false, true} {
		rep := runMerge(t, db, &tid, apply)
		if len(rep.Groups) != 0 || rep.Merged != 0 {
			t.Fatalf("re-run (apply=%v) = %+v, want no groups", apply, rep)
		}
	}
	for id, was := range before {
		now := db.LoyaltyAccount.GetX(ctx, id)
		if now.PointsBalance != was.PointsBalance || now.LifetimePoints != was.LifetimePoints || !now.UpdatedAt.Equal(was.UpdatedAt) {
			t.Errorf("account %s changed on re-run", id)
		}
	}
}

func TestMergeNeverGroupsForeignWithKenyan(t *testing.T) {
	db := openTestDB(t)
	tid := uuid.New()
	// Same nine subscriber digits, different countries.
	seedMergeAccount(t, db, tid, "0712345678", 5, 5, mergeT0, nil)
	seedMergeAccount(t, db, tid, "+256712345678", 5, 5, mergeT0, nil)
	// Numbers that do not normalise are never grouped.
	seedMergeAccount(t, db, tid, "12", 1, 1, mergeT0, nil)
	seedMergeAccount(t, db, tid, "012", 1, 1, mergeT0, nil)

	rep := runMerge(t, db, &tid, true)
	if len(rep.Groups) != 0 || rep.Merged != 0 {
		t.Fatalf("report = %+v, want no groups", rep)
	}
}

func TestMergeSurvivorFollowsPickOrder(t *testing.T) {
	db := openTestDB(t)
	contactID := uuid.New()

	// Equal lifetime points: the linked row wins over an older unlinked one.
	linkedTenant := uuid.New()
	seedMergeAccount(t, db, linkedTenant, "0712345678", 10, 10, mergeT0, nil)
	linked := seedMergeAccount(t, db, linkedTenant, "+254712345678", 10, 10, mergeT0.Add(time.Hour), &contactID)

	// Equal points, nothing linked: the oldest wins.
	oldTenant := uuid.New()
	oldest := seedMergeAccount(t, db, oldTenant, "0712345678", 10, 10, mergeT0, nil)
	seedMergeAccount(t, db, oldTenant, "+254712345678", 10, 10, mergeT0.Add(time.Hour), nil)

	// Everything equal: the lowest id wins.
	idTenant := uuid.New()
	x := seedMergeAccount(t, db, idTenant, "0712345678", 10, 10, mergeT0, nil)
	y := seedMergeAccount(t, db, idTenant, "+254712345678", 10, 10, mergeT0, nil)
	lowest := x.ID
	if strings.Compare(y.ID.String(), x.ID.String()) < 0 {
		lowest = y.ID
	}

	rep := runMerge(t, db, nil, false)
	want := map[uuid.UUID]struct {
		id     uuid.UUID
		reason string
	}{
		linkedTenant: {linked.ID, ReasonLinked},
		oldTenant:    {oldest.ID, ReasonOldest},
		idTenant:     {lowest, ReasonLowestID},
	}
	if len(rep.Groups) != len(want) {
		t.Fatalf("groups = %d, want %d: %+v", len(rep.Groups), len(want), rep.Groups)
	}
	for _, g := range rep.Groups {
		w, ok := want[g.TenantID]
		if !ok {
			t.Fatalf("unexpected tenant %s", g.TenantID)
		}
		if g.SurvivorID != w.id || g.Reason != w.reason {
			t.Errorf("tenant %s survivor = %s (%q), want %s (%q)", g.TenantID, g.SurvivorID, g.Reason, w.id, w.reason)
		}
	}
}

func TestMergeDryRunChangesNothing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	a := seedMergeAccount(t, db, tid, "0712345678", 30, 50, mergeT0, nil)
	b := seedMergeAccount(t, db, tid, "+254712345678", 100, 200, mergeT0.Add(time.Hour), nil)
	txn := db.LoyaltyTransaction.Create().
		SetTenantID(tid).SetAccountID(a.ID).SetTypeField("earn").SetPoints(30).SetBalanceAfter(30).
		SaveX(ctx)

	rep := runMerge(t, db, &tid, false)
	if !rep.DryRun || len(rep.Groups) != 1 || rep.Merged != 0 {
		t.Fatalf("report = %+v, want a dry run with one group and no merges", rep)
	}
	g := rep.Groups[0]
	if g.Status != MergeStatusWouldMerge || g.SurvivorID != b.ID || len(g.Accounts) != 2 {
		t.Errorf("group = %+v, want would_merge into %s with two accounts", g, b.ID)
	}
	if g.Phone != "*********678" {
		t.Errorf("masked phone = %q, want only the last three digits shown", g.Phone)
	}
	for _, acc := range []*ent.LoyaltyAccount{a, b} {
		now := db.LoyaltyAccount.GetX(ctx, acc.ID)
		if now.PointsBalance != acc.PointsBalance || now.LifetimePoints != acc.LifetimePoints || IsMerged(now.Metadata) {
			t.Errorf("dry run changed account %s", acc.ID)
		}
	}
	if got := db.LoyaltyTransaction.GetX(ctx, txn.ID); got.AccountID != a.ID {
		t.Error("dry run repointed a transaction")
	}
}

func TestFindAccountFollowsMergedShell(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	// The survivor is stored in an unusual spelling, so an exact lookup of the shell's spelling
	// finds only the shell. The lookup must still land on the survivor.
	shell := seedMergeAccount(t, db, tid, "0712345678", 10, 10, mergeT0, nil)
	survivor := seedMergeAccount(t, db, tid, "+254 712 345 678", 90, 90, mergeT0.Add(time.Hour), nil)

	if rep := runMerge(t, db, &tid, true); rep.Merged != 1 || rep.Groups[0].SurvivorID != survivor.ID {
		t.Fatalf("report = %+v, want a merge into %s", rep, survivor.ID)
	}
	got, err := FindAccountIn(ctx, db, tid, "0712345678")
	if err != nil {
		t.Fatalf("FindAccountIn: %v", err)
	}
	if got == nil || got.ID != survivor.ID {
		t.Errorf("FindAccountIn on the shell's spelling = %v, want survivor %s (shell %s)", got, survivor.ID, shell.ID)
	}
}

func TestReconcilerSkipsMergedShell(t *testing.T) {
	f, srv := newFakeCRM(t)
	r := newTestResolver(t, srv.URL)
	ctx := context.Background()
	tid := uuid.New()
	linked := uuid.New()
	seedMergeAccount(t, r.db, tid, "0712345678", 10, 10, mergeT0, nil)
	seedMergeAccount(t, r.db, tid, "+254712345678", 90, 90, mergeT0.Add(time.Hour), &linked)

	if rep := runMerge(t, r.db, &tid, true); rep.Merged != 1 {
		t.Fatalf("report = %+v, want one merge", rep)
	}
	if err := NewReconciler(r, nil, zap.NewNop()).RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Fatalf("reconciler linked a merged shell: %v", calls)
	}
}

func TestMaskPhone(t *testing.T) {
	cases := map[string]string{
		"+254712345678": "*********678",
		"+44791112345":  "********345",
		"12":            "**",
	}
	for in, want := range cases {
		if got := maskPhone(in); got != want {
			t.Errorf("maskPhone(%q) = %q, want %q", in, got, want)
		}
	}
}

// mergedPair seeds two accounts of one number, merges them and returns the shell and survivor.
func mergedPair(t *testing.T, db *ent.Client, tid uuid.UUID) (shell, survivor *ent.LoyaltyAccount) {
	t.Helper()
	shell = seedMergeAccount(t, db, tid, "0712345678", 30, 50, mergeT0, nil)
	survivor = seedMergeAccount(t, db, tid, "+254712345678", 100, 200, mergeT0.Add(time.Hour), nil)
	if rep := runMerge(t, db, &tid, true); rep.Merged != 1 || rep.Groups[0].SurvivorID != survivor.ID {
		t.Fatalf("report = %+v, want a merge into %s", rep, survivor.ID)
	}
	return shell, survivor
}

func TestLockAccountOnShell(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	shell, survivor := mergedPair(t, db, tid)

	tx, err := db.Tx(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	got, err := LockAccount(ctx, tx, tid, shell.ID, true)
	if err != nil || got.ID != survivor.ID {
		t.Fatalf("LockAccount(follow) = %v, %v; want survivor %s", got, err, survivor.ID)
	}
	_, err = LockAccount(ctx, tx, tid, shell.ID, false)
	var me *MergedError
	if !errors.As(err, &me) || me.AccountID != shell.ID || me.SurvivorID != survivor.ID {
		t.Fatalf("LockAccount(no follow) error = %v, want MergedError naming the survivor", err)
	}
	if got, err := LockAccount(ctx, tx, tid, survivor.ID, false); err != nil || got.ID != survivor.ID {
		t.Fatalf("LockAccount(survivor) = %v, %v; want the survivor itself", got, err)
	}
}

func TestEarnPointsOnShellCreditsSurvivor(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	shell, survivor := mergedPair(t, db, tid)
	orderID := uuid.New()

	// The caller still holds the account it resolved before the merge; the credit must land on
	// the survivor and add to its stored balance, not to the stale copy.
	res, err := EarnPoints(ctx, db, tid, shell.ID, 5, &orderID, "test earn")
	if err != nil {
		t.Fatalf("EarnPoints: %v", err)
	}
	if res.Account.ID != survivor.ID || res.Account.PointsBalance != 135 || res.Account.LifetimePoints != 255 {
		t.Errorf("credited %s with %d/%d, want survivor %s with 135/255",
			res.Account.ID, res.Account.PointsBalance, res.Account.LifetimePoints, survivor.ID)
	}
	if res.PreviousLifetime != 250 {
		t.Errorf("PreviousLifetime = %d, want 250", res.PreviousLifetime)
	}
	if res.Transaction.AccountID != survivor.ID || res.Transaction.BalanceAfter != 135 || res.Transaction.TypeField != "earn" {
		t.Errorf("transaction = %+v, want an earn on the survivor with balance 135", res.Transaction)
	}
	if got := db.LoyaltyAccount.GetX(ctx, shell.ID); got.PointsBalance != 0 || got.LifetimePoints != 0 {
		t.Errorf("shell gained points: %d/%d", got.PointsBalance, got.LifetimePoints)
	}

	ref, err := CreditPoints(ctx, db, tid, survivor.ID, 10, "referral", nil, "")
	if err != nil || ref.Account.PointsBalance != 145 || ref.Transaction.TypeField != "referral" {
		t.Errorf("CreditPoints = %+v, %v; want a referral credit to 145", ref, err)
	}
}

func TestMergeGroupReadsFreshRowsAndRefusesStalePlan(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	a := seedMergeAccount(t, db, tid, "0712345678", 30, 50, mergeT0, nil)
	b := seedMergeAccount(t, db, tid, "+254712345678", 100, 200, mergeT0.Add(time.Hour), nil)
	groups, err := duplicateGroups(ctx, db, tid)
	if err != nil || len(groups) != 1 {
		t.Fatalf("duplicateGroups = %v, %v", groups, err)
	}

	// A sale lands on the loser after the plan was made.
	db.LoyaltyAccount.UpdateOneID(a.ID).AddPointsBalance(7).AddLifetimePoints(7).ExecX(ctx)
	if _, err := mergeGroup(ctx, db, tid, groups[0], mergeT0); err != nil {
		t.Fatalf("merge after a sale: %v (the fresh read inside the transaction should absorb it)", err)
	}
	if got := db.LoyaltyAccount.GetX(ctx, b.ID); got.PointsBalance != 137 {
		t.Errorf("survivor = %d, want 137 (the sale's points carried over)", got.PointsBalance)
	}

	// Merging the same stale plan again is refused and changes nothing.
	before := db.LoyaltyAccount.GetX(ctx, b.ID)
	if _, err := mergeGroup(ctx, db, tid, groups[0], mergeT0); err == nil {
		t.Fatal("re-merging an already merged group was not refused")
	}
	if got := db.LoyaltyAccount.GetX(ctx, b.ID); got.PointsBalance != before.PointsBalance {
		t.Errorf("refused merge changed the survivor: %d -> %d", before.PointsBalance, got.PointsBalance)
	}
}

func TestMergeContactConflictKeepsBothLinks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	survivorContact, loserContact := uuid.New(), uuid.New()
	loser := seedMergeAccount(t, db, tid, "0712345678", 30, 50, mergeT0, &loserContact)
	survivor := seedMergeAccount(t, db, tid, "+254712345678", 100, 200, mergeT0.Add(time.Hour), &survivorContact)

	if rep := runMerge(t, db, &tid, false); !rep.Groups[0].ContactConflict {
		t.Errorf("dry run did not flag the contact conflict: %+v", rep.Groups[0])
	}
	rep := runMerge(t, db, &tid, true)
	if rep.Merged != 1 || !rep.Groups[0].ContactConflict {
		t.Fatalf("report = %+v, want a merged group flagged as a contact conflict", rep)
	}
	if got := db.LoyaltyAccount.GetX(ctx, survivor.ID); got.CrmContactID == nil || *got.CrmContactID != survivorContact {
		t.Errorf("survivor link = %v, want its own %s", got.CrmContactID, survivorContact)
	}
	shell := db.LoyaltyAccount.GetX(ctx, loser.ID)
	if shell.CrmContactID != nil || shell.Metadata[MergedCrmContactKey] != loserContact.String() {
		t.Errorf("shell = link %v metadata %v, want unlinked with %s kept", shell.CrmContactID, shell.Metadata, loserContact)
	}

	// Same contact on both rows is not a conflict.
	same := uuid.New()
	other := uuid.New()
	seedMergeAccount(t, db, other, "0722000111", 1, 1, mergeT0, &same)
	seedMergeAccount(t, db, other, "+254722000111", 9, 9, mergeT0, &same)
	if rep := runMerge(t, db, &other, false); rep.Groups[0].ContactConflict {
		t.Error("two rows linked to the same contact were flagged as a conflict")
	}
}

func TestFindAccountBrokenMergeChainErrors(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := uuid.New()
	// A shell whose survivor no longer exists.
	db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("0712345678").SetCustomerName("Orphan Shell").
		SetMetadata(map[string]any{MergedIntoKey: uuid.NewString()}).
		SaveX(ctx)

	got, err := FindAccountIn(ctx, db, tid, "+254712345678")
	if !errors.Is(err, ErrMergeChainBroken) || got != nil {
		t.Fatalf("FindAccountIn = %v, %v; want ErrMergeChainBroken so no caller creates a new account", got, err)
	}

	tx, err := db.Tx(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	shell, err := tx.LoyaltyAccount.Query().First(ctx)
	if err != nil {
		t.Fatalf("load shell: %v", err)
	}
	if _, err := LockAccount(ctx, tx, tid, shell.ID, true); !errors.Is(err, ErrMergeChainBroken) {
		t.Fatalf("LockAccount on an orphan shell = %v, want ErrMergeChainBroken", err)
	}
}
