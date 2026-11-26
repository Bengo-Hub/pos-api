package staffcredit

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
	"modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	"github.com/bengobox/pos-service/internal/ent/staffpurchaselink"
	"github.com/bengobox/pos-service/internal/platform/erp"
)

// pure-Go sqlite shim (duplicated per package): ent needs a driver registered as "sqlite3".
type sqlite3Driver struct{ *sqlite.Driver }

func (d sqlite3Driver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	if execer, ok := conn.(interface {
		Exec(string, []driver.Value) (driver.Result, error)
	}); ok {
		if _, err := execer.Exec("PRAGMA foreign_keys = ON;", nil); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func init() { sql.Register("sqlite3", sqlite3Driver{Driver: &sqlite.Driver{}}) }

type fakeERP struct {
	calls int
	err   error
	last  erp.StaffPurchaseRequest
}

func (f *fakeERP) Enabled() bool { return true }
func (f *fakeERP) CreateStaffPurchase(_ context.Context, _ string, req erp.StaffPurchaseRequest) (*erp.StaffPurchaseResponse, error) {
	f.calls++
	f.last = req
	if f.err != nil {
		return nil, f.err
	}
	return &erp.StaffPurchaseResponse{ID: uuid.NewString()}, nil
}

type fakeAlerts struct{ alerts []string }

func (f *fakeAlerts) PublishAlert(_ context.Context, _ uuid.UUID, alertType string, _ map[string]any) error {
	f.alerts = append(f.alerts, alertType)
	return nil
}

// Waits grow from the base with jitter and stop at the cap.
func TestRetryDelay(t *testing.T) {
	for attempt := 1; attempt <= retryMaxAttempts+2; attempt++ {
		floor := retryBase
		for i := 1; i < attempt && floor < retryCap; i++ {
			floor *= 2
		}
		if floor > retryCap {
			floor = retryCap
		}
		for i := 0; i < 30; i++ {
			if d := retryDelay(attempt); d < floor || d > retryCap || d > floor+floor/2 {
				t.Fatalf("attempt %d delay %s", attempt, d)
			}
		}
	}
}

// A failed link is re-sent when due, waits while not due, succeeds, and after the last attempt is
// failed for good with an alert; it is never tried again after that.
func TestRetryJob(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:staffcredit_retry_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	tenant := uuid.New()
	newLink := func(st syncState) *ent.StaffPurchaseLink {
		return client.StaffPurchaseLink.Create().SetTenantID(tenant).SetStaffMemberID(uuid.New()).SetUserID(uuid.New()).
			SetOrigin(staffpurchaselink.OriginCreditSale).SetSourceKey("pos:credit_sale:" + uuid.NewString()).
			SetPrincipal(decimal.NewFromInt(3000)).SetAmountSettled(decimal.Zero).SetOutstanding(decimal.NewFromInt(3000)).
			SetSyncStatus(staffpurchaselink.SyncStatusFailed).SetSyncError(st.String()).SaveX(ctx)
	}
	fe, fa := &fakeERP{err: errors.New("erp 503")}, &fakeAlerts{}
	j := &RetryJob{db: client, erp: fe, alerts: fa, log: zap.NewNop(), now: func() time.Time { return now }}

	due := newLink(syncState{Attempts: 1, NextRetryAt: now.Add(-time.Minute), InstallmentAmount: "1000.00", InstallmentsTotal: 3, EmployeeName: "Amina"})
	newLink(syncState{Attempts: 1, NextRetryAt: now.Add(time.Hour), InstallmentAmount: "1000.00", InstallmentsTotal: 3})
	newLink(syncState{}) // no resend data (an old plain-text failure): never retried

	if n, err := j.RunOnce(ctx); err != nil || n != 1 || fe.calls != 1 {
		t.Fatalf("first run tried %d (calls %d, %v), want 1", n, fe.calls, err)
	}
	if fe.last.InstallmentAmount != "1000.00" || fe.last.InstallmentsTotal != 3 || fe.last.EmployeeName != "Amina" {
		t.Errorf("resent request = %+v", fe.last)
	}
	st, _ := parseSyncState(client.StaffPurchaseLink.GetX(ctx, due.ID).SyncError)
	if st.Attempts != 2 || !st.NextRetryAt.After(now) || st.GaveUp {
		t.Errorf("after a failed retry: %+v", st)
	}

	// Run it out: every further failure moves the next try; the last one gives up and alerts.
	for i := 0; i < retryMaxAttempts; i++ {
		now = now.Add(retryCap + time.Minute)
		if _, err := j.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	st, _ = parseSyncState(client.StaffPurchaseLink.GetX(ctx, due.ID).SyncError)
	if !st.GaveUp || st.Attempts != retryMaxAttempts {
		t.Fatalf("not given up: %+v", st)
	}
	if len(fa.alerts) != 2 || fa.alerts[0] != "alert.staff_purchase_link_failed" {
		t.Errorf("alerts = %v, want one per link that gave up (2)", fa.alerts)
	}
	calls := fe.calls
	now = now.Add(48 * time.Hour)
	_, _ = j.RunOnce(ctx)
	if fe.calls != calls {
		t.Error("a link that gave up was tried again")
	}

	// Success marks the link synced.
	fe.err = nil
	ok := newLink(syncState{Attempts: 1, NextRetryAt: now.Add(-time.Minute), InstallmentAmount: "500.00", InstallmentsTotal: 6})
	if _, err := j.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if l := client.StaffPurchaseLink.GetX(ctx, ok.ID); l.SyncStatus != staffpurchaselink.SyncStatusSynced || l.ErpPurchaseID == nil || l.SyncError != "" {
		t.Errorf("successful retry = %s %v %q", l.SyncStatus, l.ErpPurchaseID, l.SyncError)
	}
}
