package scheduler

import (
	"context"
	sharedcache "github.com/Bengo-Hub/cache"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/layawayplan"
	"github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	"github.com/bengobox/pos-service/internal/modules/customers"
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
	// Runs on every replica's ticker; only the first replica in each period does the work.
	if !sharedcache.ClaimPeriod(ctx, "pos:layaway-reminders", 24*time.Hour) {
		return
	}
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
		payload := layawayPaymentDuePayload(plan, layawayCustomerContactID(ctx, s.db, plan))
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

// layawayPaymentDuePayload is the pos.layaway.payment_due payload for plan. A customer linked to
// the CRM (crmContactID set) is named by crm_contact_id alone and notifications-api reads the
// name, phone and email from the CRM; an unlinked customer, or a staff layaway, keeps
// customer_name, customer_phone and customer_email.
func layawayPaymentDuePayload(plan *ent.LayawayPlan, crmContactID *uuid.UUID) map[string]any {
	dueDate := ""
	if plan.DueDate != nil {
		dueDate = plan.DueDate.Format("2006-01-02")
	}
	return events.WithRecipient(map[string]any{
		"layaway_plan_id": plan.ID.String(),
		"balance_due":     plan.RemainingAmount,
		"due_date":        dueDate,
	}, "customer", events.Recipient{
		CrmContactID: crmContactID,
		Name:         plan.CustomerName,
		Email:        plan.CustomerEmail,
		Phone:        plan.CustomerPhone,
	})
}

// layawayCustomerContactID is the CRM contact of a customer layaway: the one on the loyalty
// account the plan was opened on, else the one on the account holding the plan's phone, both in
// the plan's own tenant. nil for a staff layaway, an unlinked customer or a failed read, so the
// reminder then carries the customer's details and still reaches them.
func layawayCustomerContactID(ctx context.Context, db *ent.Client, plan *ent.LayawayPlan) *uuid.UUID {
	if plan.PartyType == layawayplan.PartyTypeStaff {
		return nil
	}
	linked := func(acc *ent.LoyaltyAccount) *uuid.UUID {
		if acc == nil || acc.CrmContactID == nil || *acc.CrmContactID == uuid.Nil {
			return nil
		}
		id := *acc.CrmContactID
		return &id
	}
	if plan.LoyaltyAccountID != nil {
		acc, err := db.LoyaltyAccount.Query().
			Where(loyaltyaccount.ID(*plan.LoyaltyAccountID), loyaltyaccount.TenantID(plan.TenantID)).
			Only(ctx)
		if err == nil {
			if id := linked(acc); id != nil {
				return id
			}
		}
	}
	acc, err := customers.FindAccountIn(ctx, db, plan.TenantID, plan.CustomerPhone)
	if err != nil {
		return nil
	}
	return linked(acc)
}
