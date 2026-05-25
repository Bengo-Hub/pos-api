package subscriptions

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entaccount "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	entprogram "github.com/bengobox/pos-service/internal/ent/loyaltyprogram"
	entcommrule "github.com/bengobox/pos-service/internal/ent/commissionrule"
)

// SaleFinalizedSubscriber listens for pos.sale.finalized and handles:
//  1. Loyalty points auto-earn
//  2. Commission record auto-creation per service line
type SaleFinalizedSubscriber struct {
	db     *ent.Client
	logger *zap.Logger
	sub    *nats.Subscription
}

func NewSaleFinalizedSubscriber(db *ent.Client, logger *zap.Logger) *SaleFinalizedSubscriber {
	return &SaleFinalizedSubscriber{
		db:     db,
		logger: logger.Named("subscriptions.sale-finalized"),
	}
}

func (s *SaleFinalizedSubscriber) Start(conn *nats.Conn) error {
	sub, err := conn.Subscribe("pos.sale.finalized", s.handle)
	if err != nil {
		return err
	}
	s.sub = sub
	s.logger.Info("subscribed to pos.sale.finalized")
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
	s.autoCreateCommissions(ctx, tenantID, orderID, p)
}

func (s *SaleFinalizedSubscriber) autoEarnLoyalty(ctx context.Context, tenantID, orderID uuid.UUID, p saleFinalizedPayload) {
	if p.CustomerPhone == "" {
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

	account, err := s.db.LoyaltyAccount.Query().
		Where(entaccount.TenantID(tenantID), entaccount.CustomerPhone(p.CustomerPhone)).
		Only(ctx)
	if err != nil {
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
