package printing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entprintagent "github.com/bengobox/pos-service/internal/ent/printagent"
	entprintjob "github.com/bengobox/pos-service/internal/ent/printjob"
)

const (
	// JobTTL is how long a queued job stays printable. Jobs older than this expire — a receipt
	// that finally prints an hour late is worse than a re-print.
	JobTTL = 15 * time.Minute
	// ClaimLease is how long an agent holds a claimed job before it requeues (dead-agent guard).
	ClaimLease = 60 * time.Second
	// MaxAttempts is the delivery cap per job before it is marked failed.
	MaxAttempts = 3
	// AgentOnlineWindow: an agent is "online" when it polled within this window.
	AgentOnlineWindow = 90 * time.Second
)

// Queue is the server-side background print-job queue (AccuPOS remote-printing model): enqueue
// renders ESC/POS server-side; the on-site Local Print Agent polls, claims (leased), prints, acks.
type Queue struct {
	client *ent.Client
	log    *zap.Logger
	// postgres enables FOR UPDATE SKIP LOCKED claims (multi-replica safety). The sqlite test
	// driver doesn't support row locks, so tests construct the queue with postgres=false.
	postgres bool
}

// NewQueue builds the print queue service.
func NewQueue(client *ent.Client, log *zap.Logger, postgres bool) *Queue {
	return &Queue{client: client, log: log, postgres: postgres}
}

// Target is the printer snapshot stamped on a job at enqueue time.
type Target struct {
	ProfileID   string
	PrinterType string
	PrinterIP   string
	PrinterPort int
	PrinterName string
	Paper       string
}

// TargetFromProfile snapshots a printer profile into a job target.
func TargetFromProfile(p *PrinterProfile) Target {
	port := p.PrinterPort
	if port <= 0 || port > 65535 {
		port = 9100
	}
	return Target{
		ProfileID:   p.ID,
		PrinterType: p.PrinterType,
		PrinterIP:   p.PrinterIP,
		PrinterPort: port,
		PrinterName: p.PrinterName,
		Paper:       p.Paper(),
	}
}

// Printable reports whether the target can actually be dispatched by an agent.
func (t Target) Printable() bool {
	return t.PrinterIP != "" || (t.PrinterName != "" && t.PrinterName != "browser")
}

// EnqueueInput describes one job to enqueue.
type EnqueueInput struct {
	TenantID  uuid.UUID
	OutletID  uuid.UUID
	OrderID   *uuid.UUID
	JobType   string // bill | kitchen | bar | waiter | receipt | test | drawer
	Target    Target
	Payload   []byte // raw ESC/POS bytes
	DedupeKey string // optional; same (tenant, key) never enqueues twice
}

// Enqueue creates a queued job. A dedupe-key conflict is NOT an error — the existing job wins
// (idempotent retries / double-clicks never double-print).
func (q *Queue) Enqueue(ctx context.Context, in EnqueueInput) (*ent.PrintJob, error) {
	if !in.Target.Printable() {
		return nil, fmt.Errorf("printing: target has no printable destination")
	}
	create := q.client.PrintJob.Create().
		SetTenantID(in.TenantID).
		SetOutletID(in.OutletID).
		SetJobType(in.JobType).
		SetProfileID(in.Target.ProfileID).
		SetPrinterType(in.Target.PrinterType).
		SetPrinterIP(in.Target.PrinterIP).
		SetPrinterPort(in.Target.PrinterPort).
		SetPrinterName(in.Target.PrinterName).
		SetPaper(in.Target.Paper).
		SetPayloadHex(hex.EncodeToString(in.Payload))
	if in.OrderID != nil {
		create = create.SetOrderID(*in.OrderID)
	}
	if in.DedupeKey != "" {
		create = create.SetDedupeKey(in.DedupeKey)
	}
	job, err := create.Save(ctx)
	if err != nil {
		if in.DedupeKey != "" && ent.IsConstraintError(err) {
			existing, qerr := q.client.PrintJob.Query().
				Where(entprintjob.TenantID(in.TenantID), entprintjob.DedupeKey(in.DedupeKey)).
				Only(ctx)
			if qerr == nil {
				return existing, nil
			}
		}
		return nil, fmt.Errorf("printing: enqueue: %w", err)
	}
	return job, nil
}

// AgentOnline reports whether any paired, non-revoked agent for the outlet polled recently.
func (q *Queue) AgentOnline(ctx context.Context, tenantID, outletID uuid.UUID) bool {
	ok, err := q.client.PrintAgent.Query().
		Where(
			entprintagent.TenantID(tenantID),
			entprintagent.OutletID(outletID),
			entprintagent.Revoked(false),
			entprintagent.LastSeenAtGTE(time.Now().Add(-AgentOnlineWindow)),
		).
		Exist(ctx)
	return err == nil && ok
}

// GenerateAgentKey returns (plaintext, sha256hex). The plaintext is shown once at pairing.
func GenerateAgentKey() (string, string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	plaintext := "pak_" + hex.EncodeToString(raw)
	return plaintext, HashAgentKey(plaintext), nil
}

// HashAgentKey hashes a pairing key for storage/lookup.
func HashAgentKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// AuthAgent resolves a polling agent from its X-Agent-Key header value.
func (q *Queue) AuthAgent(ctx context.Context, key string) (*ent.PrintAgent, error) {
	if key == "" {
		return nil, fmt.Errorf("printing: missing agent key")
	}
	agent, err := q.client.PrintAgent.Query().
		Where(entprintagent.KeyHash(HashAgentKey(key)), entprintagent.Revoked(false)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("printing: unknown agent key")
	}
	return agent, nil
}

// touchAgent bumps last_seen_at (+version) so AgentOnline and the settings badge stay accurate.
func (q *Queue) touchAgent(ctx context.Context, agent *ent.PrintAgent, version string) {
	upd := q.client.PrintAgent.UpdateOneID(agent.ID).SetLastSeenAt(time.Now())
	if version != "" {
		upd = upd.SetVersion(version)
	}
	if _, err := upd.Save(ctx); err != nil {
		q.log.Warn("printing: touch agent failed", zap.Error(err))
	}
}

// ClaimNext long-polls for the next printable job for the agent's outlet, claiming it under a
// lease. Returns nil when no job became available within wait.
func (q *Queue) ClaimNext(ctx context.Context, agent *ent.PrintAgent, wait time.Duration, version string) (*ent.PrintJob, error) {
	q.touchAgent(ctx, agent, version)
	deadline := time.Now().Add(wait)
	for {
		job, err := q.claimOnce(ctx, agent)
		if err != nil {
			return nil, err
		}
		if job != nil {
			return job, nil
		}
		if time.Now().After(deadline) {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

// claimOnce atomically claims one job inside a transaction. Sweeps expired leases/stale jobs first.
func (q *Queue) claimOnce(ctx context.Context, agent *ent.PrintAgent) (*ent.PrintJob, error) {
	now := time.Now()

	// Inline sweeps (cheap, index-backed): requeue dead-agent leases, expire stale jobs.
	_, _ = q.client.PrintJob.Update().
		Where(
			entprintjob.StatusEQ("claimed"),
			entprintjob.ClaimExpiresAtLT(now),
		).
		SetStatus("queued").
		ClearClaimedBy().
		ClearClaimExpiresAt().
		Save(ctx)
	_, _ = q.client.PrintJob.Update().
		Where(
			entprintjob.StatusEQ("queued"),
			entprintjob.CreatedAtLT(now.Add(-JobTTL)),
		).
		SetStatus("expired").
		Save(ctx)

	tx, err := q.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("printing: claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	query := tx.PrintJob.Query().
		Where(
			entprintjob.TenantID(agent.TenantID),
			entprintjob.OutletID(agent.OutletID),
			entprintjob.StatusEQ("queued"),
			entprintjob.AttemptsLT(MaxAttempts),
		).
		Order(ent.Asc(entprintjob.FieldCreatedAt)).
		Limit(1)
	if q.postgres {
		query = query.ForUpdate(entsql.WithLockAction(entsql.SkipLocked))
	}
	job, err := query.First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("printing: claim query: %w", err)
	}

	claimed, err := tx.PrintJob.UpdateOneID(job.ID).
		SetStatus("claimed").
		SetClaimedBy(agent.ID.String()).
		SetClaimExpiresAt(now.Add(ClaimLease)).
		AddAttempts(1).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("printing: claim update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("printing: claim commit: %w", err)
	}
	return claimed, nil
}

// Ack finalizes a claimed job: printed, or failed (requeued until MaxAttempts is reached).
func (q *Queue) Ack(ctx context.Context, agent *ent.PrintAgent, jobID uuid.UUID, printed bool, errMsg string) (*ent.PrintJob, error) {
	job, err := q.client.PrintJob.Query().
		Where(
			entprintjob.ID(jobID),
			entprintjob.TenantID(agent.TenantID),
			entprintjob.ClaimedBy(agent.ID.String()),
			entprintjob.StatusEQ("claimed"),
		).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("printing: ack: job not held by agent")
	}

	upd := job.Update().ClearClaimedBy().ClearClaimExpiresAt()
	switch {
	case printed:
		upd = upd.SetStatus("printed")
	case job.Attempts >= MaxAttempts:
		upd = upd.SetStatus("failed").SetLastError(errMsg)
	default:
		upd = upd.SetStatus("queued").SetLastError(errMsg)
	}
	return upd.Save(ctx)
}
