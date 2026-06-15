package backup

import (
	"context"
	"time"

	"go.uber.org/zap"

	enttenant "github.com/bengobox/pos-service/internal/ent/tenant"
)

// schedulerAdvisoryLockKey is a fixed, service-unique key for the Postgres session advisory
// lock that guards the daily run so only ONE replica executes it. ("pos-backup")
const schedulerAdvisoryLockKey int64 = 0x504F_5342 // 'P','O','S','B'

// SchedulerConfig configures the daily auto-backup + retention churn.
type SchedulerConfig struct {
	Enabled       bool // BACKUP_SCHEDULE_ENABLED (default true)
	Hour          int  // BACKUP_SCHEDULE_HOUR (default 2) — service-local time
	RetentionDays int  // BACKUP_RETENTION_DAYS (default 4)
}

// Scheduler runs a daily auto-backup of every tenant + a retention churn, using a
// time-until-next-run timer loop (no external cron dep) and a Postgres advisory lock so
// only one replica performs the work.
type Scheduler struct {
	svc *Service
	cfg SchedulerConfig
	log *zap.Logger
}

// NewScheduler builds the scheduler. Defaults are applied for zero-value config.
func NewScheduler(svc *Service, cfg SchedulerConfig, log *zap.Logger) *Scheduler {
	if cfg.Hour < 0 || cfg.Hour > 23 {
		cfg.Hour = 2
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = DefaultRetentionDays
	}
	return &Scheduler{svc: svc, cfg: cfg, log: log.Named("backup.Scheduler")}
}

// Start launches the scheduler goroutine: a churn on startup, then backup+churn daily at
// the configured hour. Stops when ctx is cancelled.
func (sc *Scheduler) Start(ctx context.Context) {
	if !sc.cfg.Enabled {
		sc.log.Info("backup scheduler disabled (BACKUP_SCHEDULE_ENABLED=false)")
		return
	}
	sc.log.Info("backup scheduler started",
		zap.Int("hour", sc.cfg.Hour),
		zap.Int("retention_days", sc.cfg.RetentionDays))

	go func() {
		sc.runGuarded(ctx, false)
		for {
			next := nextRun(time.Now(), sc.cfg.Hour)
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				sc.runGuarded(ctx, true)
			}
		}
	}()
}

// runGuarded acquires the advisory lock and, if won, runs the daily backup (when doBackup)
// followed by the churn. Only one replica wins the lock per tick.
func (sc *Scheduler) runGuarded(ctx context.Context, doBackup bool) {
	conn, err := sc.svc.db.Conn(ctx)
	if err != nil {
		sc.log.Warn("scheduler: acquire conn failed", zap.Error(err))
		return
	}
	defer func() { _ = conn.Close() }()

	var got bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, schedulerAdvisoryLockKey).Scan(&got); err != nil {
		sc.log.Warn("scheduler: advisory lock failed", zap.Error(err))
		return
	}
	if !got {
		return
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, schedulerAdvisoryLockKey)
	}()

	if doBackup {
		sc.backupAllTenants(ctx)
	}
	if _, err := sc.svc.Churn(ctx, sc.cfg.RetentionDays); err != nil {
		sc.log.Warn("scheduler: churn failed", zap.Error(err))
	}
}

// backupAllTenants enumerates active tenants and backs each up.
func (sc *Scheduler) backupAllTenants(ctx context.Context) {
	tenants, err := sc.svc.orm.Tenant.Query().
		Where(enttenant.StatusEQ("active")).
		All(ctx)
	if err != nil {
		sc.log.Warn("scheduler: list tenants failed", zap.Error(err))
		return
	}
	ok := 0
	for _, t := range tenants {
		if _, err := sc.svc.Generate(ctx, t.ID); err != nil {
			sc.log.Warn("scheduler: tenant backup failed", zap.String("tenant", t.ID.String()), zap.Error(err))
			continue
		}
		ok++
	}
	sc.log.Info("scheduled backup complete", zap.Int("tenants", len(tenants)), zap.Int("succeeded", ok))
}

// nextRun returns the next occurrence of hour:00 strictly after now (service-local time).
func nextRun(now time.Time, hour int) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}
