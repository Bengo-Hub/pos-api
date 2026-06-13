package subscriptions

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entaccount "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	entprogram "github.com/bengobox/pos-service/internal/ent/loyaltyprogram"
	entreferral "github.com/bengobox/pos-service/internal/ent/referral"
	entcommrule "github.com/bengobox/pos-service/internal/ent/commissionrule"
	"github.com/bengobox/pos-service/internal/platform/events"
)

// SaleFinalizedSubscriber listens for pos.sale.finalized and handles:
//  1. Loyalty points auto-earn
//  2. Commission record auto-creation per service line
//  3. ERP sale_posted event (pass-through stub — wired when an ERP system is ready)
type SaleFinalizedSubscriber struct {
	db        *ent.Client
	logger    *zap.Logger
	publisher *events.Publisher
	sub       *nats.Subscription
}

func NewSaleFinalizedSubscriber(db *ent.Client, logger *zap.Logger, publisher *events.Publisher) *SaleFinalizedSubscriber {
	return &SaleFinalizedSubscriber{
		db:        db,
		logger:    logger.Named("subscriptions.sale-finalized"),
		publisher: publisher,
	}
}

func (s *SaleFinalizedSubscriber) Start(conn *nats.Conn) error {
	// QueueSubscribe (not Subscribe): with >1 pos-api replica only ONE pod in the
	// "pos-sale-finalized" group handles each event, so loyalty earn / referral /
	// commission side-effects run exactly once (this handler is not idempotent).
	sub, err := conn.QueueSubscribe("pos.sale.finalized", "pos-sale-finalized", s.handle)
	if err != nil {
		return err
	}
	s.sub = sub
	s.logger.Info("subscribed to pos.sale.finalized (queue group pos-sale-finalized)")
	return nil
}

func (s *SaleFinalizedSubscriber) Stop() {
	if s.sub != nil {
		_ = s.sub.Drain()
	}
}

type saleFinalizedPayload struct {
	TenantID   string        `json:"tenant_id"`
	OrderID    string        `json:"order_id"`
	OutletID   string        `json:"outlet_id"`
	TotalAmount float64     `json:"total_amount"`
	CustomerPhone string    `json:"customer_phone"`
	Lines      []saleLinePayload `json:"lines"`
}

type saleLinePayload struct {
	ServiceSKU     string  `json:"service_sku"`
	CatalogItemID  string  `json:"catalog_item_id"`
	StaffMemberID  string  `json:"staff_member_id"`
	SaleAmount     float64 `json:"sale_amount"`
}

func (s *SaleFinalizedSubscriber) handle(msg *nats.Msg) {
	var wrapper struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &wrapper); err != nil {
		s.logger.Warn("sale.finalized: bad envelope", zap.Error(err))
		return
	}

	var p saleFinalizedPayload
	if err := json.Unmarshal(wrapper.Payload, &p); err != nil {
		s.logger.Warn("sale.finalized: bad payload", zap.Error(err))
		return
	}

	tenantID, err := uuid.Parse(p.TenantID)
	if err != nil {
		return
	}
	orderID, _ := uuid.Parse(p.OrderID)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	s.autoEarnLoyalty(ctx, tenantID, orderID, p)
	s.autoClaimReferral(ctx, tenantID, orderID, p)
	s.autoCreateCommissions(ctx, tenantID, orderID, p)
	// ERP sale_posted: pass-through stub — no-op until an ERP system is integrated.
	// When an ERP integration is ready, wire the ERP client call here and remove this comment.
	if s.publisher != nil {
		_ = s.publisher.PublishERPSalePosted(ctx, tenantID, map[string]any{
			"order_id":     p.OrderID,
			"outlet_id":    p.OutletID,
			"total_amount": p.TotalAmount,
		})
	}
}

// phoneDigits strips every non-digit from a phone string.
func phoneDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// nationalSubscriberDigits returns the last 9 digits of a phone (the Kenyan national
// subscriber number) so "+254 743 793901", "0743793901" and "254743793901" all collapse
// to the same key. Returns "" when fewer than 9 digits are present.
func nationalSubscriberDigits(s string) string {
	d := phoneDigits(s)
	if len(d) < 9 {
		return ""
	}
	return d[len(d)-9:]
}

// isWalkInPhone reports whether a phone is the anonymous walk-in sentinel (all zeros / blank).
// Walk-in (customerless) sales must NEVER earn or redeem loyalty — only customers who supplied a
// real number that maps to a registered account do. See recordCreditSale's walk-in fallback.
func isWalkInPhone(phone string) bool {
	d := phoneDigits(phone)
	if d == "" {
		return true
	}
	for _, c := range d {
		if c != '0' {
			return false
		}
	}
	return true // all-zero sentinel (e.g. +000000000000)
}

// resolveLoyaltyAccount finds the tenant's loyalty account for a phone, tolerating format
// differences. It tries an exact match first (fast path), then falls back to matching on the
// 9-digit national subscriber number so an order stored as "+254 743 793901" still credits an
// account stored as "0743793901". Returns nil (no error) when no account exists.
func (s *SaleFinalizedSubscriber) resolveLoyaltyAccount(ctx context.Context, tenantID uuid.UUID, phone string) *ent.LoyaltyAccount {
	if acc, err := s.db.LoyaltyAccount.Query().
		Where(entaccount.TenantID(tenantID), entaccount.CustomerPhone(phone)).
		First(ctx); err == nil && acc != nil {
		return acc
	}
	if nat := nationalSubscriberDigits(phone); nat != "" {
		if acc, err := s.db.LoyaltyAccount.Query().
			Where(entaccount.TenantID(tenantID), entaccount.CustomerPhoneHasSuffix(nat)).
			First(ctx); err == nil && acc != nil {
			return acc
		}
	}
	return nil
}

func (s *SaleFinalizedSubscriber) autoEarnLoyalty(ctx context.Context, tenantID, orderID uuid.UUID, p saleFinalizedPayload) {
	// Walk-in / anonymous sales (no phone, or the all-zero walk-in sentinel) never earn loyalty.
	if p.CustomerPhone == "" || isWalkInPhone(p.CustomerPhone) {
		return
	}

	prog, err := s.db.LoyaltyProgram.Query().
		Where(entprogram.TenantID(tenantID), entprogram.IsActive(true)).
		First(ctx)
	if err != nil || prog == nil {
		return
	}

	pointsEarned := int(math.Floor(p.TotalAmount * prog.EarnRate))
	if pointsEarned <= 0 {
		return
	}

	// Only customers who are REGISTERED for the loyalty program earn — resolve the existing account
	// (format-tolerant). We deliberately do NOT auto-create an account here: a sale with an
	// unrecognised phone simply earns nothing.
	account := s.resolveLoyaltyAccount(ctx, tenantID, p.CustomerPhone)
	if account == nil {
		s.logger.Debug("sale.finalized: no loyalty account for phone, skipping earn",
			zap.String("phone", p.CustomerPhone))
		return
	}

	newBalance := account.PointsBalance + pointsEarned
	newLifetime := account.LifetimePoints + pointsEarned

	_, err = s.db.LoyaltyAccount.UpdateOneID(account.ID).
		SetPointsBalance(newBalance).
		SetLifetimePoints(newLifetime).
		Save(ctx)
	if err != nil {
		s.logger.Error("sale.finalized: failed to update loyalty balance", zap.Error(err))
		return
	}

	_, err = s.db.LoyaltyTransaction.Create().
		SetTenantID(tenantID).
		SetAccountID(account.ID).
		SetOrderID(orderID).
		SetTypeField("earn").
		SetPoints(pointsEarned).
		SetBalanceAfter(newBalance).
		SetNotes("Auto-earn on sale").
		Save(ctx)
	if err != nil {
		s.logger.Error("sale.finalized: failed to create loyalty transaction", zap.Error(err))
	}

	// Notify the customer they earned points (notifications-service → WhatsApp/SMS "You earned X pts").
	if s.publisher != nil {
		_ = s.publisher.PublishLoyaltyPointsEarned(ctx, tenantID, map[string]any{
			"account_id":     account.ID.String(),
			"customer_phone": account.CustomerPhone,
			"customer_name":  account.CustomerName,
			"points_earned":  pointsEarned,
			"balance_after":  newBalance,
			"order_id":       orderID.String(),
		})
	}

	// Publish tier upgrade event if the customer crossed a tier threshold.
	oldTier := evaluateTier(prog.TierThresholds, account.LifetimePoints)
	newTier := evaluateTier(prog.TierThresholds, newLifetime)
	if oldTier != newTier && s.publisher != nil {
		crmContactID := ""
		if account.CrmContactID != nil {
			crmContactID = account.CrmContactID.String()
		}
		_ = s.publisher.PublishLoyaltyTierUpgraded(ctx, tenantID, map[string]any{
			"account_id":      account.ID.String(),
			"customer_phone":  account.CustomerPhone,
			"customer_name":   account.CustomerName,
			"crm_contact_id":  crmContactID,
			"old_tier":        oldTier,
			"new_tier":        newTier,
			"lifetime_points": newLifetime,
			"program_id":      prog.ID.String(),
		})
		s.logger.Info("loyalty tier upgraded",
			zap.String("phone", account.CustomerPhone),
			zap.String("old_tier", oldTier),
			zap.String("new_tier", newTier),
		)
	}
}

// autoClaimReferral credits the referrer bonus points when a referred friend's sale finalizes.
// It matches the buyer's phone against any PENDING referral's referred_phone, credits the referrer
// loyalty account, records a "referral" transaction, marks the referral earned, and emits
// loyalty.referral_earned. No-op when the buyer has no pending referral.
func (s *SaleFinalizedSubscriber) autoClaimReferral(ctx context.Context, tenantID, orderID uuid.UUID, p saleFinalizedPayload) {
	if p.CustomerPhone == "" || isWalkInPhone(p.CustomerPhone) {
		return
	}
	// Match the pending referral format-tolerantly: exact first, then by national subscriber number.
	ref, err := s.db.Referral.Query().
		Where(
			entreferral.TenantID(tenantID),
			entreferral.ReferredPhone(p.CustomerPhone),
			entreferral.StatusEQ("pending"),
		).
		First(ctx)
	if (err != nil || ref == nil) && nationalSubscriberDigits(p.CustomerPhone) != "" {
		ref, err = s.db.Referral.Query().
			Where(
				entreferral.TenantID(tenantID),
				entreferral.ReferredPhoneHasSuffix(nationalSubscriberDigits(p.CustomerPhone)),
				entreferral.StatusEQ("pending"),
			).
			First(ctx)
	}
	if err != nil || ref == nil {
		return // no pending referral for this buyer
	}

	referrer, err := s.db.LoyaltyAccount.Get(ctx, ref.ReferrerAccountID)
	if err != nil {
		return
	}

	newBalance := referrer.PointsBalance + ref.BonusPoints
	newLifetime := referrer.LifetimePoints + ref.BonusPoints
	if ref.BonusPoints > 0 {
		if _, err := s.db.LoyaltyAccount.UpdateOneID(referrer.ID).
			SetPointsBalance(newBalance).
			SetLifetimePoints(newLifetime).
			Save(ctx); err != nil {
			s.logger.Error("referral: failed to credit referrer", zap.Error(err))
			return
		}
	}

	var txnID *uuid.UUID
	if ref.BonusPoints > 0 {
		if txn, err := s.db.LoyaltyTransaction.Create().
			SetTenantID(tenantID).
			SetAccountID(referrer.ID).
			SetOrderID(orderID).
			SetTypeField("referral").
			SetPoints(ref.BonusPoints).
			SetBalanceAfter(newBalance).
			SetNotes("Referral bonus: " + p.CustomerPhone).
			Save(ctx); err == nil {
			txnID = &txn.ID
		}
	}

	upd := s.db.Referral.UpdateOneID(ref.ID).SetStatus("earned").SetEarnedAt(time.Now())
	if txnID != nil {
		upd = upd.SetEarnTransactionID(*txnID)
	}
	if referred := s.resolveLoyaltyAccount(ctx, tenantID, p.CustomerPhone); referred != nil {
		upd = upd.SetReferredAccountID(referred.ID)
	}
	if _, err := upd.Save(ctx); err != nil {
		s.logger.Error("referral: failed to mark earned", zap.Error(err))
		return
	}

	if s.publisher != nil {
		crmContactID := ""
		if referrer.CrmContactID != nil {
			crmContactID = referrer.CrmContactID.String()
		}
		_ = s.publisher.PublishLoyaltyReferralEarned(ctx, tenantID, map[string]any{
			"referral_id":      ref.ID.String(),
			"referrer_account": referrer.ID.String(),
			"referrer_phone":   referrer.CustomerPhone,
			"referrer_name":    referrer.CustomerName,
			"crm_contact_id":   crmContactID,
			"referred_phone":   p.CustomerPhone,
			"bonus_points":     ref.BonusPoints,
			"balance_after":    newBalance,
		})
	}
	s.logger.Info("referral earned",
		zap.String("referrer", referrer.CustomerPhone),
		zap.String("referred", p.CustomerPhone),
		zap.Int("bonus", ref.BonusPoints),
	)
}

// evaluateTier returns the highest tier name the customer qualifies for based on lifetime points.
// The program's tier_thresholds map encodes "tier_name" → min_lifetime_points.
// Returns "member" when no threshold is defined or none are reached.
func evaluateTier(thresholds map[string]any, lifetimePoints int) string {
	best := ""
	bestMin := -1
	for tier, raw := range thresholds {
		var minPts int
		switch v := raw.(type) {
		case float64:
			minPts = int(v)
		case int:
			minPts = v
		}
		if lifetimePoints >= minPts && minPts > bestMin {
			bestMin = minPts
			best = tier
		}
	}
	if best == "" {
		return "member"
	}
	return best
}

func (s *SaleFinalizedSubscriber) autoCreateCommissions(ctx context.Context, tenantID, orderID uuid.UUID, p saleFinalizedPayload) {
	if len(p.Lines) == 0 {
		return
	}

	activeRules, err := s.db.CommissionRule.Query().
		Where(entcommrule.TenantID(tenantID), entcommrule.IsActive(true)).
		All(ctx)
	if err != nil || len(activeRules) == 0 {
		return
	}

	for _, line := range p.Lines {
		staffID, err := uuid.Parse(line.StaffMemberID)
		if err != nil || line.SaleAmount <= 0 {
			continue
		}
		catalogItemID, _ := uuid.Parse(line.CatalogItemID)

		var matchedRule *ent.CommissionRule
		for _, rule := range activeRules {
			// Staff-specific + item-specific is the most specific match
			staffMatch := rule.StaffMemberID == nil || *rule.StaffMemberID == staffID
			itemMatch := rule.CatalogItemID == nil || *rule.CatalogItemID == catalogItemID
			if staffMatch && itemMatch {
				if matchedRule == nil {
					matchedRule = rule
				} else {
					// Prefer more specific: staff+item > staff-only > item-only > global
					prevSpecificity := specificity(matchedRule, staffID, catalogItemID)
					currSpecificity := specificity(rule, staffID, catalogItemID)
					if currSpecificity > prevSpecificity {
						matchedRule = rule
					}
				}
			}
		}

		if matchedRule == nil {
			continue
		}

		commissionAmount := 0.0
		switch matchedRule.RuleType {
		case "flat":
			if matchedRule.FlatAmount != nil {
				commissionAmount = *matchedRule.FlatAmount
			}
		case "percentage":
			if matchedRule.Percentage != nil {
				commissionAmount = line.SaleAmount * (*matchedRule.Percentage / 100.0)
			}
		}

		if commissionAmount <= 0 {
			continue
		}

		rate := 0.0
		if matchedRule.Percentage != nil {
			rate = *matchedRule.Percentage
		}

		_, err = s.db.CommissionRecord.Create().
			SetTenantID(tenantID).
			SetStaffMemberID(staffID).
			SetOrderID(orderID).
			SetServiceSku(line.ServiceSKU).
			SetSaleAmount(line.SaleAmount).
			SetCommissionRate(rate).
			SetCommissionAmount(commissionAmount).
			SetStatus("pending").
			Save(ctx)
		if err != nil {
			s.logger.Error("sale.finalized: failed to create commission record",
				zap.String("staff_id", staffID.String()), zap.Error(err))
		}
	}
}

func specificity(rule *ent.CommissionRule, staffID, catalogItemID uuid.UUID) int {
	score := 0
	if rule.StaffMemberID != nil && *rule.StaffMemberID == staffID {
		score += 2
	}
	if rule.CatalogItemID != nil && *rule.CatalogItemID == catalogItemID {
		score += 1
	}
	return score
}
