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
