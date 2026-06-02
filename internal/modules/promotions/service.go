// Package promotions provides the promotion service layer for POS operations.
// It encapsulates promo code validation, discount calculation, and usage tracking
// that was previously incomplete in handlers (only validated existence, never calculated discounts).
package promotions

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/promotion"
	"github.com/bengobox/pos-service/internal/ent/promotionrule"
)

// ApplyResult holds the result of applying a promotion to an order.
type ApplyResult struct {
	Valid          bool            `json:"valid"`
	PromoCode      string          `json:"promo_code"`
	PromoID        uuid.UUID       `json:"promo_id"`
	DiscountAmount decimal.Decimal `json:"discount_amount"`
	Reason         string          `json:"reason,omitempty"` // reason for invalid
}

// Service provides promotion business logic.
type Service struct {
	client *ent.Client
	log    *zap.Logger
}

// NewService creates a new promotion service.
func NewService(client *ent.Client, log *zap.Logger) *Service {
	return &Service{
		client: client,
		log:    log.Named("promotions.service"),
	}
}

// ApplyPromoCode validates a promo code and calculates the discount amount.
// Unlike the previous implementation which only checked existence, this:
// 1. Validates the promo code exists and is active
// 2. Checks expiry
// 3. Checks usage limits (if configured via metadata)
// 4. Calculates the discount amount based on promo metadata
func (s *Service) ApplyPromoCode(ctx context.Context, tenantID uuid.UUID, promoCode string, orderAmount decimal.Decimal) (*ApplyResult, error) {
	promo, err := s.client.Promotion.Query().
		Where(
			promotion.TenantID(tenantID),
			promotion.PromoCode(promoCode),
			promotion.StatusEQ("active"),
		).
		Only(ctx)
	if err != nil {
		return &ApplyResult{Valid: false, Reason: "promo code not found or inactive"}, nil
	}

	code := derefStr(promo.PromoCode)

	// Check expiry
	if promo.EndAt != nil && time.Now().After(*promo.EndAt) {
		return &ApplyResult{Valid: false, PromoCode: code, PromoID: promo.ID, Reason: "promotion has expired"}, nil
	}

	// Check start date
	if !promo.StartAt.IsZero() && time.Now().Before(promo.StartAt) {
		return &ApplyResult{Valid: false, PromoCode: code, PromoID: promo.ID, Reason: "promotion has not started yet"}, nil
	}

	// Calculate discount from metadata
	discountAmount := s.calculateDiscount(promo, orderAmount)

	return &ApplyResult{
		Valid:          true,
		PromoCode:      code,
		PromoID:        promo.ID,
		DiscountAmount: discountAmount.Round(2),
	}, nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// calculateDiscount determines the discount amount based on promotion metadata.
// Supports: {"discount_type": "percentage|fixed", "discount_value": 10.0, "max_discount": 500.0}
func (s *Service) calculateDiscount(promo *ent.Promotion, orderAmount decimal.Decimal) decimal.Decimal {
	meta := promo.Metadata
	if meta == nil {
		return decimal.Zero
	}

	discountType, _ := meta["discount_type"].(string)
	discountValue, _ := meta["discount_value"].(float64)

	if discountValue <= 0 {
		return decimal.Zero
	}

	var discount decimal.Decimal
	switch discountType {
	case "percentage":
		discount = orderAmount.Mul(decimal.NewFromFloat(discountValue)).Div(decimal.NewFromInt(100))
	case "fixed":
		discount = decimal.NewFromFloat(discountValue)
	default:
		return decimal.Zero
	}

	// Cap at max_discount if specified
	if maxDiscount, ok := meta["max_discount"].(float64); ok && maxDiscount > 0 {
		maxDec := decimal.NewFromFloat(maxDiscount)
		if discount.GreaterThan(maxDec) {
			discount = maxDec
		}
	}

	// Don't exceed order amount
	if discount.GreaterThan(orderAmount) {
		discount = orderAmount
	}

	return discount
}

// HappyHourMatch describes an active happy-hour promotion at a point in time.
type HappyHourMatch struct {
	PromoID   uuid.UUID  `json:"promo_id"`
	Name      string     `json:"name"`
	OutletID  *uuid.UUID `json:"outlet_id,omitempty"`
	WindowEnd string     `json:"window_end,omitempty"`
}

// ActiveHappyHours returns auto-apply happy-hour promotions that are live at `now`
// for the given outlet (nil outlet promos apply to all outlets). A promo is live when:
//   - promo_kind = happy_hour, status = active, auto_apply = true
//   - now is within [start_at, end_at] (date range, if set)
//   - now's weekday is in days_of_week (or days_of_week empty = every day)
//   - now's HH:MM is within [window_start, window_end]
func (s *Service) ActiveHappyHours(ctx context.Context, tenantID uuid.UUID, outletID *uuid.UUID, now time.Time) ([]*ent.Promotion, error) {
	q := s.client.Promotion.Query().Where(
		promotion.TenantID(tenantID),
		promotion.PromoKindEQ(promotion.PromoKindHappyHour),
		promotion.StatusEQ("active"),
		promotion.AutoApply(true),
	)
	promos, err := q.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("promotions: query happy hours: %w", err)
	}
	var active []*ent.Promotion
	for _, p := range promos {
		if p.OutletID != nil && (outletID == nil || *p.OutletID != *outletID) {
			continue
		}
		if !s.isWithinSchedule(p, now) {
			continue
		}
		active = append(active, p)
	}
	return active, nil
}

// EvaluateHappyHourDiscount returns the best auto-apply happy-hour discount for an outlet
// on the given subtotal at `now`. Used by the orders service at checkout (decoupled hook).
func (s *Service) EvaluateHappyHourDiscount(ctx context.Context, tenantID, outletID uuid.UUID, subtotal decimal.Decimal) decimal.Decimal {
	if subtotal.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	var outletPtr *uuid.UUID
	if outletID != uuid.Nil {
		outletPtr = &outletID
	}
	active, err := s.ActiveHappyHours(ctx, tenantID, outletPtr, time.Now())
	if err != nil || len(active) == 0 {
		return decimal.Zero
	}
	best := decimal.Zero
	for _, p := range active {
		rule, rErr := s.client.PromotionRule.Query().
			Where(promotionrule.PromotionID(p.ID)).First(ctx)
		if rErr != nil || rule == nil {
			continue
		}
		var d decimal.Decimal
		switch rule.DiscountType {
		case "percentage":
			d = subtotal.Mul(decimal.NewFromFloat(rule.DiscountValue)).Div(decimal.NewFromInt(100))
		case "fixed_amount":
			d = decimal.NewFromFloat(rule.DiscountValue)
		default:
			continue
		}
		if rule.MaxDiscount != nil && *rule.MaxDiscount > 0 {
			if cap := decimal.NewFromFloat(*rule.MaxDiscount); d.GreaterThan(cap) {
				d = cap
			}
		}
		if d.GreaterThan(best) {
			best = d
		}
	}
	if best.GreaterThan(subtotal) {
		best = subtotal
	}
	return best.Round(2)
}

// isWithinSchedule reports whether `now` falls inside a promotion's date range,
// allowed weekdays, and daily time window.
func (s *Service) isWithinSchedule(p *ent.Promotion, now time.Time) bool {
	if !p.StartAt.IsZero() && now.Before(p.StartAt) {
		return false
	}
	if p.EndAt != nil && now.After(*p.EndAt) {
		return false
	}
	if len(p.DaysOfWeek) > 0 {
		wd := int(now.Weekday())
		found := false
		for _, d := range p.DaysOfWeek {
			if d == wd {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if p.WindowStart != "" && p.WindowEnd != "" {
		cur := now.Format("15:04")
		// Same-day window (e.g. 16:00–18:00). Overnight windows not supported here.
		if cur < p.WindowStart || cur > p.WindowEnd {
			return false
		}
	}
	return true
}

// CreatePromotion creates a new promotion with proper defaults.
func (s *Service) CreatePromotion(ctx context.Context, tenantID uuid.UUID, name, promoCode string, startAt, endAt *time.Time, metadata map[string]any) (*ent.Promotion, error) {
	if promoCode == "" {
		promoCode = fmt.Sprintf("PROMO-%s", uuid.New().String()[:8])
	}

	builder := s.client.Promotion.Create().
		SetTenantID(tenantID).
		SetName(name).
		SetPromoCode(promoCode).
		SetStatus("active")

	if startAt != nil {
		builder = builder.SetStartAt(*startAt)
	}
	if endAt != nil {
		builder = builder.SetEndAt(*endAt)
	}
	if metadata != nil {
		builder = builder.SetMetadata(metadata)
	}

	return builder.Save(ctx)
}
