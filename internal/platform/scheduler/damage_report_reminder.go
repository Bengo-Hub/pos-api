package scheduler

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/roomdamagereport"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// damageReportSLAHours is how long a damage report can sit "pending" before it's flagged as
// overdue for manager review. A stale report risks the guest checking out before the charge
// can be posted to their (still-active) folio — see ApproveDamageReport's folio_posted=false
// fallback for what happens once that window has passed.
const damageReportSLAHours = 24

// DamageReportReminderScheduler publishes hotel.damage.overdue once for each RoomDamageReport
// that has sat "pending" past the SLA window — a metadata["overdue_notified"] flag makes this
// idempotent (fires once per report, not every run). Runs once at startup, then hourly.
type DamageReportReminderScheduler struct {
	log       *zap.Logger
	db        *ent.Client
	publisher *events.Publisher
}

func NewDamageReportReminderScheduler(log *zap.Logger, db *ent.Client, publisher *events.Publisher) *DamageReportReminderScheduler {
	return &DamageReportReminderScheduler{log: log, db: db, publisher: publisher}
}

func (s *DamageReportReminderScheduler) Start(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	s.run(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.run(ctx)
		}
	}
}

func (s *DamageReportReminderScheduler) run(ctx context.Context) {
	cutoff := time.Now().Add(-damageReportSLAHours * time.Hour)
	reports, err := s.db.RoomDamageReport.Query().
		Where(
			roomdamagereport.StatusEQ(roomdamagereport.StatusPending),
			roomdamagereport.CreatedAtLT(cutoff),
		).
		All(ctx)
	if err != nil {
		s.log.Error("damage report reminder: query failed", zap.Error(err))
		return
	}

	notified := 0
	for _, report := range reports {
		if flagged, _ := report.Metadata["overdue_notified"].(bool); flagged {
			continue
		}
		meta := map[string]any{}
		for k, v := range report.Metadata {
			meta[k] = v
		}
		meta["overdue_notified"] = true
		if _, err := report.Update().SetMetadata(meta).Save(ctx); err != nil {
			s.log.Warn("damage report reminder: flag update failed", zap.Stringer("report_id", report.ID), zap.Error(err))
			continue
		}
		if s.publisher != nil {
			_ = s.publisher.PublishHotelDamageOverdue(ctx, report.TenantID, map[string]any{
				"damage_report_id": report.ID,
				"room_id":          report.RoomID,
				"amount":           report.Amount,
				"currency":         report.Currency,
				"hours_pending":    int(time.Since(report.CreatedAt).Hours()),
			})
		}
		notified++
	}
	if notified > 0 {
		s.log.Info("damage report overdue reminders published", zap.Int("count", notified))
	}
}
