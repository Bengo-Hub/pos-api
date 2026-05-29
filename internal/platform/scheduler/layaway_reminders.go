package scheduler

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/layawayplan"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// LayawayReminderScheduler fires pos.layaway.payment_due for each active plan
// whose due_date falls tomorrow. Runs once per day at startup and then every 24 hours.
type LayawayReminderScheduler struct {
	log       *zap.Logger
	db        *ent.Client
	publisher *events.Publisher
}

func NewLayawayReminderScheduler(log *zap.Logger, db *ent.Client, publisher *events.Publisher) *LayawayReminderScheduler {
	return &LayawayReminderScheduler{log: log, db: db, publisher: publisher}
}

// Start launches the background ticker. Call in a goroutine from main.
func (s *LayawayReminderScheduler) Start(ctx context.Context) {
	ticker := time.NewTicker(24 * time.Hour)
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

func (s *LayawayReminderScheduler) run(ctx context.Context) {
	now := time.Now()
	tomorrowStart := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	tomorrowEnd := tomorrowStart.Add(24 * time.Hour)

	plans, err := s.db.LayawayPlan.Query().
		Where(
			layawayplan.StatusEQ("active"),
			layawayplan.DueDateGTE(tomorrowStart),
			layawayplan.DueDateLT(tomorrowEnd),
		).
		All(ctx)
	if err != nil {
		s.log.Error("layaway reminder query failed", zap.Error(err))
		return
	}

	for _, plan := range plans {
		dueDate := ""
		if plan.DueDate != nil {
			dueDate = plan.DueDate.Format("2006-01-02")
		}
		payload := map[string]any{
			"layaway_plan_id": plan.ID,
			"customer_name":   plan.CustomerName,
			"customer_phone":  plan.CustomerPhone,
			"balance_due":     plan.RemainingAmount,
			"due_date":        dueDate,
		}
		if err := s.publisher.PublishLayawayPaymentDue(ctx, uuid.UUID(plan.TenantID), payload); err != nil {
			s.log.Warn("failed to publish layaway.payment_due",
				zap.Stringer("plan_id", plan.ID),
				zap.Error(err),
			)
		}
	}
	if len(plans) > 0 {
		s.log.Info("layaway payment_due reminders published", zap.Int("count", len(plans)))
	}
}
