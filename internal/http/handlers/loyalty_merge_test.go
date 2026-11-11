package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/customers"
)

func TestParseMergeRequest(t *testing.T) {
	tid := uuid.NewString()
	cases := []struct {
		name      string
		body      string
		wantErr   bool
		wantApply bool
		wantAll   bool
	}{
		{name: "empty body is a dry run over every tenant", body: "", wantAll: true},
		{name: "dry run for one tenant", body: `{"tenant_id":"` + tid + `"}`},
		{name: "real run for one tenant", body: `{"tenant_id":"` + tid + `","dry_run":false}`, wantApply: true},
		{name: "real run for every tenant needs the flag", body: `{"dry_run":false,"all_tenants":true}`, wantApply: true, wantAll: true},
		{name: "real run without a scope is refused", body: `{"dry_run":false}`, wantErr: true},
		{name: "wrong tenant type is refused, never a fleet run", body: `{"dry_run":false,"tenant_id":123}`, wantErr: true},
		{name: "bad tenant id", body: `{"tenant_id":"not-a-uuid"}`, wantErr: true},
		{name: "unknown field (typo) is refused", body: `{"dryrun":false}`, wantErr: true},
		{name: "broken json", body: `{"dry_run":false`, wantErr: true},
		{name: "trailing data", body: `{"tenant_id":"` + tid + `"} {"dry_run":false}`, wantErr: true},
		{name: "tenant and all_tenants together", body: `{"tenant_id":"` + tid + `","all_tenants":true}`, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/recovery/loyalty-merge-duplicates", strings.NewReader(c.body))
			opts, problem := parseMergeRequest(req)
			if (problem != "") != c.wantErr {
				t.Fatalf("problem = %q, want error %v", problem, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if opts.Apply != c.wantApply {
				t.Errorf("Apply = %v, want %v", opts.Apply, c.wantApply)
			}
			if (opts.TenantID == nil) != c.wantAll {
				t.Errorf("TenantID = %v, want every tenant %v", opts.TenantID, c.wantAll)
			}
		})
	}
}

func TestMergeEndpointRefusesBadBody(t *testing.T) {
	db := openSQLiteTestClient(t, "loyalty_merge_bad_body")
	ctx := context.Background()
	tid := uuid.New()
	a := db.LoyaltyAccount.Create().SetTenantID(tid).SetCustomerPhone("0712345678").SetCustomerName("A").
		SetPointsBalance(5).SetLifetimePoints(5).SaveX(ctx)
	db.LoyaltyAccount.Create().SetTenantID(tid).SetCustomerPhone("+254712345678").SetCustomerName("B").
		SetPointsBalance(9).SetLifetimePoints(9).SaveX(ctx)

	h := NewLoyaltyMergeHandler(db, zap.NewNop())
	rec := httptest.NewRecorder()
	h.Merge(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"dry_run": false, "tenant_id": 123}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := db.LoyaltyAccount.GetX(ctx, a.ID); customers.IsMerged(got.Metadata) || got.PointsBalance != 5 {
		t.Error("a refused request merged accounts")
	}
}

// mergedShell seeds two accounts of one number in the handler test database and merges them.
func mergedShell(t *testing.T, db *ent.Client, tid uuid.UUID) (shell, survivor *ent.LoyaltyAccount) {
	t.Helper()
	ctx := context.Background()
	t0 := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)
	shell = db.LoyaltyAccount.Create().SetTenantID(tid).SetCustomerPhone("0712345678").SetCustomerName("Shell").
		SetPointsBalance(30).SetLifetimePoints(50).SetCreatedAt(t0).SaveX(ctx)
	survivor = db.LoyaltyAccount.Create().SetTenantID(tid).SetCustomerPhone("+254712345678").SetCustomerName("Survivor").
		SetPointsBalance(100).SetLifetimePoints(200).SetCreatedAt(t0.Add(time.Hour)).SaveX(ctx)
	rep, err := customers.MergeDuplicateAccounts(ctx, db, customers.MergeOptions{TenantID: &tid, Apply: true}, t0)
	if err != nil || rep.Merged != 1 {
		t.Fatalf("merge = %+v, %v", rep, err)
	}
	return shell, survivor
}

func TestEarnOnMergedShellCreditsSurvivor(t *testing.T) {
	db := openSQLiteTestClient(t, "loyalty_earn_shell")
	ctx := context.Background()
	tid := uuid.New()
	shell, survivor := mergedShell(t, db, tid)
	h := NewLoyaltyHandler(zap.NewNop(), db, nil)

	balance, ltx, err := h.applyEarn(ctx, tid, shell.ID, 5, nil, "")
	if err != nil {
		t.Fatalf("applyEarn: %v", err)
	}
	if balance != 135 || ltx.AccountID != survivor.ID {
		t.Errorf("earn = balance %d on %s, want 135 on survivor %s", balance, ltx.AccountID, survivor.ID)
	}
	if got := db.LoyaltyAccount.GetX(ctx, shell.ID); got.PointsBalance != 0 {
		t.Errorf("shell balance = %d, want 0", got.PointsBalance)
	}
}

func TestRedeemOnMergedShell(t *testing.T) {
	db := openSQLiteTestClient(t, "loyalty_redeem_shell")
	ctx := context.Background()
	tid := uuid.New()
	shell, survivor := mergedShell(t, db, tid)
	h := NewLoyaltyHandler(zap.NewNop(), db, nil)

	// Staff path: the till named the shell, so it is told to look the customer up again.
	_, _, err := h.applyRedeem(ctx, tid, shell.ID, 10, nil, "", false)
	var me *customers.MergedError
	if !errors.As(err, &me) || me.SurvivorID != survivor.ID {
		t.Fatalf("applyRedeem(no follow) = %v, want MergedError naming the survivor", err)
	}
	rec := httptest.NewRecorder()
	if !respondMergedError(rec, err) || rec.Code != http.StatusConflict {
		t.Fatalf("respondMergedError status = %d, want 409", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != loyaltyAccountMergedCode || body["survivor_account_id"] != survivor.ID.String() {
		t.Errorf("409 body = %v, want the merged code and the survivor id", body)
	}
	if got := db.LoyaltyAccount.GetX(ctx, survivor.ID); got.PointsBalance != 130 {
		t.Errorf("refused redeem changed the survivor: %d", got.PointsBalance)
	}

	// The pre-read check answers the same way before any transaction opens.
	rec = httptest.NewRecorder()
	if !respondAccountMerged(rec, db.LoyaltyAccount.GetX(ctx, shell.ID)) || rec.Code != http.StatusConflict {
		t.Fatalf("respondAccountMerged status = %d, want 409", rec.Code)
	}
	if respondAccountMerged(httptest.NewRecorder(), db.LoyaltyAccount.GetX(ctx, survivor.ID)) {
		t.Error("a live account was reported as merged")
	}

	// S2S path: resolved by phone, so the shell is followed and the survivor is debited.
	balance, ltx, err := h.applyRedeem(ctx, tid, shell.ID, 10, nil, "", true)
	if err != nil || balance != 120 || ltx.AccountID != survivor.ID {
		t.Fatalf("applyRedeem(follow) = %d on %v, %v; want 120 on the survivor", balance, ltx, err)
	}
}

func TestFindOrCreateAfterSurvivorDeleted(t *testing.T) {
	db := openSQLiteTestClient(t, "loyalty_orphan_shell")
	ctx := context.Background()
	tid := uuid.New()
	shell, survivor := mergedShell(t, db, tid)
	// Deleted by a path that left the shell behind.
	db.LoyaltyAccount.DeleteOneID(survivor.ID).ExecX(ctx)
	h := NewLoyaltyHandler(zap.NewNop(), db, customers.NewResolver(db, nil, zap.NewNop()))

	// Earn on the explicit shell id refuses: it never credits a row nobody can find.
	if _, _, err := h.applyEarn(ctx, tid, shell.ID, 5, nil, ""); !errors.Is(err, customers.ErrMergeChainBroken) {
		t.Fatalf("applyEarn on an orphan shell = %v, want ErrMergeChainBroken", err)
	}

	// The phone is not locked out: find-or-create gets a usable account back.
	acc, err := h.findOrCreateAccountByPhone(ctx, tid, "+254712345678", "Jane Doe")
	if err != nil || acc == nil {
		t.Fatalf("findOrCreateAccountByPhone = %v, %v; want an account", acc, err)
	}
	if customers.IsMerged(acc.Metadata) {
		t.Errorf("returned account %s is still a merged shell", acc.ID)
	}
	if balance, _, err := h.applyEarn(ctx, tid, acc.ID, 5, nil, ""); err != nil || balance != 5 {
		t.Errorf("earn after release = %d, %v; want 5", balance, err)
	}
}

func TestListAccountsHidesMergedShells(t *testing.T) {
	db := openSQLiteTestClient(t, "loyalty_list_shells")
	ctx := context.Background()
	tid := uuid.New()
	shell, survivor := mergedShell(t, db, tid)

	ids, err := db.LoyaltyAccount.Query().Where(customers.AccountNotMerged()).IDs(ctx)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(ids) != 1 || ids[0] != survivor.ID {
		t.Errorf("live accounts = %v, want only the survivor (shell %s hidden)", ids, shell.ID)
	}
}
