package staffcredit

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/staffpurchaselink"
	"github.com/bengobox/pos-service/internal/platform/erp"
)

// A link whose push to erp failed is retried in the background. Its retry state lives in the
// link's sync_error as JSON (no schema change): the attempts so far, when the next try is due,
// what to resend, and the last error. After the last attempt the link is failed for good and an
// alert is published, so a purchase is never retried forever nor lost silently.
const (
	retryMaxAttempts = 8
	retryBase        = 5 * time.Minute
	retryCap         = 6 * time.Hour
	retryBatch       = 50
	retryInterval    = 10 * time.Minute
	retryCallTimeout = 20 * time.Second
)

// syncState is the JSON kept in sync_error.
type syncState struct {
	Error             string    `json:"error"`
	Attempts          int       `json:"attempts"`
	NextRetryAt       time.Time `json:"next_retry_at"`
	GaveUp            bool      `json:"gave_up,omitempty"`
	InstallmentAmount string    `json:"installment_amount"`
	InstallmentsTotal int       `json:"installments_total"`
	EmployeeName      string    `json:"employee_name,omitempty"`
}

func parseSyncState(s string) (syncState, bool) {
	var st syncState
	if err := json.Unmarshal([]byte(s), &st); err != nil || st.InstallmentAmount == "" {
		return st, false
	}
	return st, true
}

func (st syncState) String() string {
	raw, _ := json.Marshal(st)
	return string(raw)
}

// retryDelay is the wait after the attempt-th failed push: exponential from retryBase with up to
// 50% jitter, never above retryCap.
func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := retryBase
	for i := 1; i < attempt && d < retryCap; i++ {
		d *= 2
	}
	d += time.Duration(rand.Int63n(int64(d)/2 + 1))
	if d > retryCap {
		d = retryCap
	}
	return d
}

// nextState records one more failed push: either the next retry time or, after the last
// attempt, the end.
func nextState(st syncState, err error, now time.Time) syncState {
	st.Attempts++
	st.Error = err.Error()
	if st.Attempts >= retryMaxAttempts {
		st.GaveUp = true
		st.NextRetryAt = time.Time{}
		return st
	}
	st.NextRetryAt = now.Add(retryDelay(st.Attempts))
	return st
}

// AlertPublisher publishes a pos alert (events.Publisher.PublishAlert).
type AlertPublisher interface {
	PublishAlert(ctx context.Context, tenantID uuid.UUID, alertType string, data map[string]any) error
}

// erpPusher sends a staff purchase to erp (erp.Client).
type erpPusher interface {
	Enabled() bool
	CreateStaffPurchase(ctx context.Context, tenantID string, req erp.StaffPurchaseRequest) (*erp.StaffPurchaseResponse, error)
}

// RetryJob re-sends failed staff purchase links to erp.
type RetryJob struct {
	db     *ent.Client
	erp    erpPusher
	alerts AlertPublisher
	log    *zap.Logger
	now    func() time.Time
	claim  func(ctx context.Context, key string, ttl time.Duration) bool
}

// NewRetryJob builds the job; alerts may be nil.
func NewRetryJob(db *ent.Client, erpClient *erp.Client, alerts AlertPublisher, log *zap.Logger) *RetryJob {
	return &RetryJob{db: db, erp: erpClient, alerts: alerts, log: log.Named("staffcredit.retry"), now: time.Now, claim: sharedcache.ClaimPeriod}
}

// Start runs the job every retryInterval until ctx ends; one replica runs each period.
func (j *RetryJob) Start(ctx context.Context) {
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !j.claim(ctx, "pos:staff-purchase-retry", retryInterval) {
				continue
			}
			if n, err := j.RunOnce(ctx); err != nil {
				j.log.Error("staff purchase retry run failed", zap.Error(err))
			} else if n > 0 {
				j.log.Info("staff purchase links retried", zap.Int("links", n))
			}
		}
	}
}

// RunOnce retries up to retryBatch failed links whose next try is due, oldest first. Returns how
// many it tried.
func (j *RetryJob) RunOnce(ctx context.Context) (int, error) {
	if j.erp == nil || !j.erp.Enabled() {
		return 0, nil
	}
	links, err := j.db.StaffPurchaseLink.Query().
		Where(staffpurchaselink.SyncStatusEQ(staffpurchaselink.SyncStatusFailed), staffpurchaselink.StatusEQ(staffpurchaselink.StatusActive)).
		Order(ent.Asc(staffpurchaselink.FieldUpdatedAt)).Limit(retryBatch * 4).All(ctx)
	if err != nil {
		return 0, err
	}
	now := j.now()
	tried := 0
	for _, l := range links {
		if tried >= retryBatch || ctx.Err() != nil {
			break
		}
		st, ok := parseSyncState(l.SyncError)
		if !ok || st.GaveUp || now.Before(st.NextRetryAt) {
			continue
		}
		tried++
		j.retry(ctx, l, st, now)
	}
	return tried, nil
}

func (j *RetryJob) retry(ctx context.Context, l *ent.StaffPurchaseLink, st syncState, now time.Time) {
	ref := ""
	switch {
	case l.LayawayPlanID != nil:
		ref = l.LayawayPlanID.String()
	case l.PosOrderID != nil:
		ref = l.PosOrderID.String()
	}
	install, _ := decimal.NewFromString(st.InstallmentAmount)
	cctx, cancel := context.WithTimeout(ctx, retryCallTimeout)
	resp, err := j.erp.CreateStaffPurchase(cctx, l.TenantID.String(), erp.StaffPurchaseRequest{
		AuthUserID: l.UserID.String(), Origin: string(l.Origin), POSReference: ref, SourceKey: l.SourceKey,
		Principal: l.Principal.StringFixed(2), InstallmentAmount: install.StringFixed(2),
		InstallmentsTotal: st.InstallmentsTotal, EmployeeName: st.EmployeeName,
	})
	cancel()
	if err == nil {
		upd := l.Update().SetSyncStatus(staffpurchaselink.SyncStatusSynced).SetSyncError("")
		if resp != nil {
			if id, perr := uuid.Parse(resp.ID); perr == nil {
				upd = upd.SetErpPurchaseID(id)
			}
		}
		if serr := upd.Exec(ctx); serr != nil {
			j.log.Error("staff purchase synced but not recorded", zap.String("source_key", l.SourceKey), zap.Error(serr))
		}
		return
	}
	st = nextState(st, err, now)
	if serr := l.Update().SetSyncError(st.String()).Exec(ctx); serr != nil {
		j.log.Error("staff purchase retry state not saved", zap.String("source_key", l.SourceKey), zap.Error(serr))
	}
	if !st.GaveUp {
		return
	}
	j.log.Error("staff purchase gave up after the last attempt", zap.String("source_key", l.SourceKey),
		zap.Int("attempts", st.Attempts), zap.String("error", st.Error))
	if j.alerts != nil {
		_ = j.alerts.PublishAlert(ctx, l.TenantID, "alert.staff_purchase_link_failed", map[string]any{
			"link_id": l.ID.String(), "source_key": l.SourceKey, "user_id": l.UserID.String(),
			"principal": l.Principal.StringFixed(2), "attempts": st.Attempts, "error": st.Error,
			"message": fmt.Sprintf("Staff purchase %s could not reach payroll after %d attempts. Re-send it from staff credit or record it in HR.", l.SourceKey, st.Attempts),
		})
	}
}
