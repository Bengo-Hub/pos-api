package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/Bengo-Hub/pagination"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/promotion"
	"github.com/bengobox/pos-service/internal/ent/promotionrule"
	promotions "github.com/bengobox/pos-service/internal/modules/promotions"
)

// PromotionHandler handles promotion management endpoints.
type PromotionHandler struct {
	log      *zap.Logger
	client   *ent.Client
	promoSvc *promotions.Service
}

func NewPromotionHandler(log *zap.Logger, client *ent.Client, promoSvc *promotions.Service) *PromotionHandler {
	return &PromotionHandler{log: log, client: client, promoSvc: promoSvc}
}

// ListPromotions handles GET /{tenantID}/pos/promotions
func (h *PromotionHandler) ListPromotions(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	query := h.client.Promotion.Query().Where(promotion.TenantID(tid))

	if status := r.URL.Query().Get("status"); status != "" {
		query = query.Where(promotion.Status(status))
	} else {
		query = query.Where(promotion.Status("active"))
	}

	p := pagination.Parse(r)
	total, _ := query.Clone().Count(r.Context())
	promos, err := query.Order(ent.Desc(promotion.FieldStartAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list promotions failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, pagination.NewResponse(promos, total, p))
}

type createPromoInput struct {
	PromoCode  string     `json:"promoCode"`
	Name       string     `json:"name"`
	StartAt    *time.Time `json:"startAt"`
	EndAt      *time.Time `json:"endAt"`
	UsageLimit int        `json:"usageLimit"`
	// Happy-hour / auto-apply fields
	PromoKind   string `json:"promo_kind"`   // code | happy_hour | auto
	OutletID    string `json:"outlet_id"`    // optional outlet scope
	DaysOfWeek  []int  `json:"days_of_week"` // 0=Sun..6=Sat
	WindowStart string `json:"window_start"` // HH:MM
	WindowEnd   string `json:"window_end"`   // HH:MM
	AutoApply   bool   `json:"auto_apply"`
	// Discount rule
	ScopeType     string   `json:"scope_type"`    // all | category | item
	ScopeIDs      []string `json:"scope_ids"`     // inventory category ids / skus
	DiscountType  string   `json:"discount_type"` // percentage | fixed_amount | fixed_price
	DiscountValue float64  `json:"discount_value"`
	MaxDiscount   *float64 `json:"max_discount"`
	MealPeriod    string   `json:"meal_period"` // optional: breakfast|am_break|lunch|pm_break|dinner (negotiated meal rate)
}

// CreatePromotion handles POST /{tenantID}/pos/promotions
func (h *PromotionHandler) CreatePromotion(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createPromoInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if input.PromoCode == "" {
		input.PromoCode = "PROMO-" + uuid.New().String()[:8]
	}

	builder := h.client.Promotion.Create().
		SetTenantID(tid).
		SetName(input.Name).
		SetPromoCode(input.PromoCode).
		SetAutoApply(input.AutoApply).
		SetStatus("active")
	if input.PromoKind != "" {
		builder = builder.SetPromoKind(promotion.PromoKind(input.PromoKind))
	}
	if input.WindowStart != "" {
		builder = builder.SetWindowStart(input.WindowStart)
	}
	if input.WindowEnd != "" {
		builder = builder.SetWindowEnd(input.WindowEnd)
	}
	if len(input.DaysOfWeek) > 0 {
		builder = builder.SetDaysOfWeek(input.DaysOfWeek)
	}
	if oid, perr := uuid.Parse(input.OutletID); perr == nil {
		builder = builder.SetOutletID(oid)
	}
	if input.StartAt != nil {
		builder.SetStartAt(*input.StartAt)
	}
	if input.EndAt != nil {
		builder.SetEndAt(*input.EndAt)
	}
	promo, err := builder.Save(r.Context())
	if err != nil {
		h.log.Error("create promotion failed", zap.Error(err))
		jsonError(w, "failed to create promotion: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Persist the discount rule when provided (scope + discount type/value/cap).
	if input.DiscountValue > 0 || input.ScopeType != "" {
		scopeType := input.ScopeType
		if scopeType == "" {
			scopeType = "all"
		}
		discountType := input.DiscountType
		if discountType == "" {
			discountType = "percentage"
		}
		rb := h.client.PromotionRule.Create().
			SetPromotionID(promo.ID).
			SetRuleType("discount").
			SetScopeType(promotionrule.ScopeType(scopeType)).
			SetScopeIds(input.ScopeIDs).
			SetDiscountType(promotionrule.DiscountType(discountType)).
			SetDiscountValue(input.DiscountValue)
		if input.MaxDiscount != nil {
			rb = rb.SetMaxDiscount(*input.MaxDiscount)
		}
		if input.MealPeriod != "" {
			rb = rb.SetMealPeriod(promotionrule.MealPeriod(input.MealPeriod))
		}
		if input.ScopeType == "" && len(input.ScopeIDs) > 0 {
			rb = rb.SetScopeType(promotionrule.ScopeTypeItem)
		}
		if _, rerr := rb.Save(r.Context()); rerr != nil {
			h.log.Error("create promotion rule failed", zap.Error(rerr))
		}
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, promo)
}

// GetActiveHappyHours handles GET /{tenantID}/pos/promotions/happy-hour/active —
// returns auto-apply happy-hour promotions live right now for the request's outlet.
func (h *PromotionHandler) GetActiveHappyHours(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var outletID *uuid.UUID
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, perr := uuid.Parse(oidStr); perr == nil {
			outletID = &oid
		}
	}
	active, err := h.promoSvc.ActiveHappyHours(r.Context(), tid, outletID, time.Now())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, active)
}

// ApplyPromoCode handles POST /{tenantID}/pos/promotions/apply
func (h *PromotionHandler) ApplyPromoCode(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input struct {
		PromoCode string  `json:"promoCode"`
		OrderID   string  `json:"orderId"`
		Amount    float64 `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	orderAmount := decimal.NewFromFloat(input.Amount)
	result, err := h.promoSvc.ApplyPromoCode(r.Context(), tid, input.PromoCode, orderAmount)
	if err != nil {
		h.log.Error("apply promo code failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if !result.Valid {
		jsonOK(w, map[string]any{
			"valid":  false,
			"reason": result.Reason,
		})
		return
	}

	jsonOK(w, map[string]any{
		"valid":          true,
		"promoCode":      result.PromoCode,
		"promoId":        result.PromoID,
		"discountAmount": result.DiscountAmount.StringFixed(2),
	})
}
