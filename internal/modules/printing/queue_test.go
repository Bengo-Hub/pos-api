package printing

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
)

// pure-Go sqlite shim: ent expects driver "sqlite3"; modernc.org/sqlite (no cgo) registers as
// "sqlite". Same bridge as the handlers tests (held_items_test.go).
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

func init() {
	sql.Register("sqlite3", sqlite3Driver{Driver: &sqlite.Driver{}})
}

func newTestQueue(t *testing.T) (*Queue, *ent.Client) {
	t.Helper()
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:printq_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	// postgres=false: sqlite has no FOR UPDATE SKIP LOCKED
	return NewQueue(client, zap.NewNop(), false), client
}

func pairTestAgent(t *testing.T, client *ent.Client, tenantID, outletID uuid.UUID) (*ent.PrintAgent, string) {
	t.Helper()
	plaintext, hash, err := GenerateAgentKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	agent, err := client.PrintAgent.Create().
		SetTenantID(tenantID).
		SetOutletID(outletID).
		SetName("test agent").
		SetKeyHash(hash).
		Save(context.Background())
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return agent, plaintext
}

func testTarget() Target {
	return Target{ProfileID: "customer", PrinterType: "network", PrinterIP: "192.168.0.50", PrinterPort: 9100, Paper: "80mm"}
}

func TestEnqueueDedupe(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()
	tid, oid := uuid.New(), uuid.New()

	in := EnqueueInput{TenantID: tid, OutletID: oid, JobType: "bill", Target: testTarget(), Payload: []byte{0x1B, 0x40}, DedupeKey: "order1:bill:customer"}
	j1, err := q.Enqueue(ctx, in)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	j2, err := q.Enqueue(ctx, in)
	if err != nil {
		t.Fatalf("dedupe enqueue: %v", err)
	}
	if j1.ID != j2.ID {
		t.Fatalf("dedupe key created a second job: %s vs %s", j1.ID, j2.ID)
	}
}

func TestEnqueueRejectsUnprintableTarget(t *testing.T) {
	q, _ := newTestQueue(t)
	_, err := q.Enqueue(context.Background(), EnqueueInput{
		TenantID: uuid.New(), OutletID: uuid.New(), JobType: "bill",
		Target: Target{ProfileID: "customer", PrinterType: "browser"}, Payload: []byte{0x1B},
	})
	if err == nil {
		t.Fatal("expected error for browser-only target")
	}
}

func TestClaimAckPrinted(t *testing.T) {
	q, client := newTestQueue(t)
	ctx := context.Background()
	tid, oid := uuid.New(), uuid.New()
	agent, key := pairTestAgent(t, client, tid, oid)

	authed, err := q.AuthAgent(ctx, key)
	if err != nil || authed.ID != agent.ID {
		t.Fatalf("auth agent: %v", err)
	}
	if _, err := q.AuthAgent(ctx, "pak_wrong"); err == nil {
		t.Fatal("expected auth failure for bad key")
	}

	job, err := q.Enqueue(ctx, EnqueueInput{TenantID: tid, OutletID: oid, JobType: "test", Target: testTarget(), Payload: []byte{0x1B, 0x40}})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	claimed, err := q.ClaimNext(ctx, agent, 0, "1.2.0")
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v (job=%v)", err, claimed)
	}
	if claimed.ID != job.ID || claimed.Status != "claimed" || claimed.Attempts != 1 {
		t.Fatalf("unexpected claim state: %+v", claimed)
	}

	// AgentOnline flips true after the poll touched last_seen_at.
	if !q.AgentOnline(ctx, tid, oid) {
		t.Fatal("agent should be online after polling")
	}

	acked, err := q.Ack(ctx, agent, job.ID, true, "")
	if err != nil || acked.Status != "printed" {
		t.Fatalf("ack printed: %v (%+v)", err, acked)
	}

	// Nothing left to claim.
	none, err := q.ClaimNext(ctx, agent, 0, "")
	if err != nil || none != nil {
		t.Fatalf("expected empty queue, got %v (%v)", none, err)
	}
}

func TestAckFailureRequeuesUntilCap(t *testing.T) {
	q, client := newTestQueue(t)
	ctx := context.Background()
	tid, oid := uuid.New(), uuid.New()
	agent, _ := pairTestAgent(t, client, tid, oid)

	job, err := q.Enqueue(ctx, EnqueueInput{TenantID: tid, OutletID: oid, JobType: "bill", Target: testTarget(), Payload: []byte{0x1B}})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		claimed, cErr := q.ClaimNext(ctx, agent, 0, "")
		if cErr != nil || claimed == nil {
			t.Fatalf("claim attempt %d: %v", attempt, cErr)
		}
		acked, aErr := q.Ack(ctx, agent, job.ID, false, "printer offline")
		if aErr != nil {
			t.Fatalf("ack attempt %d: %v", attempt, aErr)
		}
		if attempt < MaxAttempts && acked.Status != "queued" {
			t.Fatalf("attempt %d: expected requeue, got %s", attempt, acked.Status)
		}
		if attempt == MaxAttempts && acked.Status != "failed" {
			t.Fatalf("attempt %d: expected failed, got %s", attempt, acked.Status)
		}
	}
}

func TestExpiredLeaseRequeues(t *testing.T) {
	q, client := newTestQueue(t)
	ctx := context.Background()
	tid, oid := uuid.New(), uuid.New()
	agent, _ := pairTestAgent(t, client, tid, oid)

	job, err := q.Enqueue(ctx, EnqueueInput{TenantID: tid, OutletID: oid, JobType: "bill", Target: testTarget(), Payload: []byte{0x1B}})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if claimed, cErr := q.ClaimNext(ctx, agent, 0, ""); cErr != nil || claimed == nil {
		t.Fatalf("claim: %v", cErr)
	}

	// Simulate the agent dying mid-job: force the lease into the past.
	if _, uErr := client.PrintJob.UpdateOneID(job.ID).
		SetClaimExpiresAt(time.Now().Add(-time.Minute)).
		Save(ctx); uErr != nil {
		t.Fatalf("age lease: %v", uErr)
	}

	reclaimed, err := q.ClaimNext(ctx, agent, 0, "")
	if err != nil || reclaimed == nil || reclaimed.ID != job.ID {
		t.Fatalf("expected lease-expired job to requeue and reclaim, got %v (%v)", reclaimed, err)
	}
}

func TestStaleQueuedJobExpires(t *testing.T) {
	q, client := newTestQueue(t)
	ctx := context.Background()
	tid, oid := uuid.New(), uuid.New()
	agent, _ := pairTestAgent(t, client, tid, oid)

	// Create a queued job already older than the TTL (created_at is immutable after create,
	// but settable at create time).
	job, err := client.PrintJob.Create().
		SetTenantID(tid).
		SetOutletID(oid).
		SetJobType("bill").
		SetPrinterType("network").
		SetPrinterIP("192.168.0.50").
		SetPayloadHex("1b40").
		SetCreatedAt(time.Now().Add(-JobTTL - time.Minute)).
		Save(ctx)
	if err != nil {
		t.Fatalf("create stale job: %v", err)
	}

	got, err := q.ClaimNext(ctx, agent, 0, "")
	if err != nil || got != nil {
		t.Fatalf("expected no claim for expired job, got %v (%v)", got, err)
	}
	refreshed, _ := client.PrintJob.Get(ctx, job.ID)
	if refreshed.Status != "expired" {
		t.Fatalf("expected expired, got %s", refreshed.Status)
	}
}
