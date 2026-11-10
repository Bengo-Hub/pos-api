package customers

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

func TestReconcilerSkipsUnlinked(t *testing.T) {
	f, srv := newFakeCRM(t)
	r := newTestResolver(t, srv.URL)
	ctx := context.Background()
	tid := uuid.New()
	marked := map[string]any{UnlinkedAtKey: "2026-11-01T10:00:00Z"}

	deletedAcc := r.db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("+254712345678").SetCustomerName("Deleted In CRM").
		SetMetadata(marked).
		SaveX(ctx)
	deletedClient := r.db.ClientRecord.Create().
		SetTenantID(tid).SetPhone("+254712345678").SetMetadata(marked).
		SaveX(ctx)
	other := r.db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("+254722000111").SetCustomerName("Never Linked").
		SetMetadata(map[string]any{"source": "import"}).
		SaveX(ctx)

	if err := NewReconciler(r, nil, zap.NewNop()).RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	calls := f.calls()
	if len(calls) != 1 || calls[0]["phone"] != "+254722000111" {
		t.Fatalf("upsert calls = %v, want only the unmarked account", calls)
	}
	if got := r.db.LoyaltyAccount.GetX(ctx, deletedAcc.ID); got.CrmContactID != nil || !IsUnlinked(got.Metadata) {
		t.Error("a deleted-contact account was re-linked")
	}
	if got := r.db.ClientRecord.GetX(ctx, deletedClient.ID); got.CrmContactID != nil || !IsUnlinked(got.Metadata) {
		t.Error("a deleted-contact client record was re-linked")
	}
	if got := r.db.LoyaltyAccount.GetX(ctx, other.ID); got.CrmContactID == nil {
		t.Error("an unmarked account was not linked")
	}
}

func TestReconcilerLinksUnlinked(t *testing.T) {
	f, srv := newFakeCRM(t)
	r := newTestResolver(t, srv.URL)
	ctx := context.Background()
	tid := uuid.New()
	alreadyLinked := uuid.New()
	base := time.Now().Add(-time.Hour)

	oldest, err := r.db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("0712345678").SetCustomerName("Oldest Customer").
		SetPointsBalance(40).SetCreatedAt(base).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed oldest account: %v", err)
	}
	newer, err := r.db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("+254722000111").SetCustomerName("Newer Customer").
		SetCreatedAt(base.Add(time.Minute)).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed newer account: %v", err)
	}
	linked, err := r.db.LoyaltyAccount.Create().
		SetTenantID(tid).SetCustomerPhone("+254733000222").SetCustomerName("Linked Customer").
		SetCrmContactID(alreadyLinked).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed linked account: %v", err)
	}
	client, err := r.db.ClientRecord.Create().
		SetTenantID(tid).SetPhone("0744000333").
		Save(ctx)
	if err != nil {
		t.Fatalf("seed client record: %v", err)
	}

	if err := NewReconciler(r, nil, zap.NewNop()).RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	calls := f.calls()
	if len(calls) != 3 {
		t.Fatalf("made %d upsert calls, want 3 (two accounts and one client record)", len(calls))
	}
	if calls[0]["phone"] != "+254712345678" || calls[1]["phone"] != "+254722000111" {
		t.Errorf("accounts were not linked oldest first: %v then %v", calls[0]["phone"], calls[1]["phone"])
	}

	gotOldest := r.db.LoyaltyAccount.GetX(ctx, oldest.ID)
	if gotOldest.CrmContactID == nil {
		t.Fatal("oldest account was not linked")
	}
	if gotOldest.CustomerPhone != "+254712345678" {
		t.Errorf("oldest account phone = %q, want the CRM's +254712345678", gotOldest.CustomerPhone)
	}
	if gotOldest.PointsBalance != 40 {
		t.Errorf("oldest account points = %d, want 40 kept", gotOldest.PointsBalance)
	}
	if got := r.db.LoyaltyAccount.GetX(ctx, newer.ID); got.CrmContactID == nil {
		t.Error("newer account was not linked")
	}
	if got := r.db.LoyaltyAccount.GetX(ctx, linked.ID); got.CrmContactID == nil || *got.CrmContactID != alreadyLinked {
		t.Error("an already linked account was changed")
	}
	gotClient := r.db.ClientRecord.GetX(ctx, client.ID)
	if gotClient.CrmContactID == nil {
		t.Fatal("client record was not linked")
	}
	if gotClient.Phone != "+254744000333" {
		t.Errorf("client phone = %q, want +254744000333", gotClient.Phone)
	}
}
