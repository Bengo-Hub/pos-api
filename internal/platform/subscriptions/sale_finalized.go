package subscriptions

import (
	"context"
	"encoding/json"
	sharedcache "github.com/Bengo-Hub/cache"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entcommrule "github.com/bengobox/pos-service/internal/ent/commissionrule"
	entprogram "github.com/bengobox/pos-service/internal/ent/loyaltyprogram"
	entreferral "github.com/bengobox/pos-service/internal/ent/referral"
	"github.com/bengobox/pos-service/internal/modules/customers"
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
	// commission side-effects run once per delivery; handle() also claims each order once,
	// which covers republished events.
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
	TenantID      string            `json:"tenant_id"`
	OrderID       string            `json:"order_id"`
	OutletID      string            `json:"outlet_id"`
	TotalAmount   float64           `json:"total_amount"`
	CustomerPhone string            `json:"customer_phone"`
	Lines         []saleLinePayload `json:"lines"`
}

type saleLinePayload struct {
	ServiceSKU    string  `json:"service_sku"`
	CatalogItemID string  `json:"catalog_item_id"`
	StaffMemberID string  `json:"staff_member_id"`
	SaleAmount    float64 `json:"sale_amount"`
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

	// Loyalty earn, referral claim and commissions are not idempotent, and the event can
	// arrive twice (SaleFinalizedReconciler republishes an order whose publish looked failed
	// or missing). One claim per order makes the side effects run once.
	if orderID != uuid.Nil && !sharedcache.ClaimOnce(ctx, "pos:sale-finalized:"+orderID.String(), 30*24*time.Hour) {
		s.logger.Info("sale.finalized: already processed, skipping", zap.String("order_id", p.OrderID))
		return
	}

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
// differences (see customers.FindAccountIn). Returns nil when no account exists.
func (s *SaleFinalizedSubscriber) resolveLoyaltyAccount(ctx context.Context, tenantID uuid.UUID, phone string) *ent.LoyaltyAccount {
	acc, err := customers.FindAccountIn(ctx, s.db, tenantID, phone)
	if err != nil {
		s.logger.Warn("sale.finalized: loyalty account lookup failed", zap.Error(err))
		return nil
	}
	return acc
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
		s.logger.Debug("sale.finalized: no loyalty account for the buyer's phone, skipping earn",
			zap.String("tenant_id", tenantID.String()), zap.String("order_id", orderID.String()))
		return
	}

	// Locked, atomic credit (customers.EarnPoints): the old read-then-SET wrote back a balance
	// read before the update, which could wipe points added in between (a concurrent earn, or
	// the duplicate account merge crediting this account). A merged shell is followed to its
	// survivor so the sale never credits an emptied duplicate.
	res, err := customers.EarnPoints(ctx, s.db, tenantID, account.ID, pointsEarned, &orderID, "Auto-earn on sale")
	if err != nil {
		s.logger.Error("sale.finalized: failed to credit loyalty points", zap.Error(err))
		return
	}
	previousLifetime := res.PreviousLifetime
	account = res.Account
	newBalance := account.PointsBalance
	newLifetime := account.LifetimePoints

	// Notify the customer they earned points (notifications-service → WhatsApp/SMS "You earned X pts").
	if s.publisher != nil {
		payload := events.LoyaltyPointsEarnedPayload(account, pointsEarned, newBalance)
		payload["order_id"] = orderID.String()
		_ = s.publisher.PublishLoyaltyPointsEarned(ctx, tenantID, payload)
	}

	// Publish tier upgrade event if the customer crossed a tier threshold.
	oldTier := evaluateTier(prog.TierThresholds, previousLifetime)
	newTier := evaluateTier(prog.TierThresholds, newLifetime)
	if oldTier != newTier && s.publisher != nil {
		_ = s.publisher.PublishLoyaltyTierUpgraded(ctx, tenantID, tierUpgradedPayload(account, prog.ID, oldTier, newTier, newLifetime))
		s.logger.Info("loyalty tier upgraded",
			zap.String("tenant_id", tenantID.String()),
			zap.String("account_id", account.ID.String()),
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

	newBalance := referrer.PointsBalance
	var txnID *uuid.UUID
	if ref.BonusPoints > 0 {
		// Locked, atomic credit that follows a merged shell (see autoEarnLoyalty).
		res, err := customers.CreditPoints(ctx, s.db, tenantID, referrer.ID, ref.BonusPoints, "referral", &orderID,
			"Referral bonus: "+p.CustomerPhone)
		if err != nil {
			s.logger.Error("referral: failed to credit referrer", zap.Error(err))
			return
		}
		referrer = res.Account
		newBalance = referrer.PointsBalance
		txnID = &res.Transaction.ID
	}

	upd := s.db.Referral.UpdateOneID(ref.ID).SetStatus("earned").SetEarnedAt(time.Now())
	if txnID != nil {
		upd = upd.SetEarnTransactionID(*txnID)
	}
	referred := s.resolveLoyaltyAccount(ctx, tenantID, p.CustomerPhone)
	if referred != nil {
		upd = upd.SetReferredAccountID(referred.ID)
	}
	if _, err := upd.Save(ctx); err != nil {
		s.logger.Error("referral: failed to mark earned", zap.Error(err))
		return
	}

	if s.publisher != nil {
		_ = s.publisher.PublishLoyaltyReferralEarned(ctx, tenantID, referralEarnedPayload(ref, referrer, referred, p.CustomerPhone, newBalance))
	}
	s.logger.Info("referral earned",
		zap.String("tenant_id", tenantID.String()),
		zap.String("referral_id", ref.ID.String()),
		zap.String("referrer_account", referrer.ID.String()),
		zap.Int("bonus", ref.BonusPoints),
	)
}

// tierUpgradedPayload is the pos.loyalty.tier_upgraded payload. A linked account is named by
// crm_contact_id only (marketflow updates that contact, notifications-api looks it up); an
// unlinked one keeps customer_name, customer_email and customer_phone (see events.WithRecipient).
func tierUpgradedPayload(acc *ent.LoyaltyAccount, programID uuid.UUID, oldTier, newTier string, lifetime int) map[string]any {
	return events.WithRecipient(map[string]any{
		"account_id":      acc.ID.String(),
		"old_tier":        oldTier,
		"new_tier":        newTier,
		"lifetime_points": lifetime,
		"program_id":      programID.String(),
	}, "customer", events.LoyaltyAccountRecipient(acc))
}

// referralEarnedPayload is the pos.loyalty.referral_earned payload. The notice thanks the
// REFERRER, so crm_contact_id is always the referrer's contact, never the referred friend's.
// The friend is named by referred_crm_contact_id when their account is linked, else by
// referred_phone as before. referred may be nil (the friend has no loyalty account).
func referralEarnedPayload(ref *ent.Referral, referrer, referred *ent.LoyaltyAccount, referredPhone string, balanceAfter int) map[string]any {
	p := events.WithRecipient(map[string]any{
		"referral_id":      ref.ID.String(),
		"referrer_account": referrer.ID.String(),
		"bonus_points":     ref.BonusPoints,
		"balance_after":    balanceAfter,
	}, "referrer", events.LoyaltyAccountRecipient(referrer))
	if referred != nil && referred.CrmContactID != nil && *referred.CrmContactID != uuid.Nil {
		p["referred_crm_contact_id"] = referred.CrmContactID.String()
	} else {
		p["referred_phone"] = referredPhone
	}
	return p
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
