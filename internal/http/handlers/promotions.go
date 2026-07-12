package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
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

// promotionWithRule is a Promotion embedding its discount PromotionRule (nil if none was
// ever created) — the item picker, discount summary, and edit form on the frontend all
// need the rule's scope_ids/discount_type/discount_value/buy_get fields alongside the
// promotion itself, so every promotion-returning endpoint serializes this shape instead of
// the bare ent.Promotion (which has no formal edge to PromotionRule to eager-load).
type promotionWithRule struct {
	*ent.Promotion
	Rule *ent.PromotionRule `json:"rule"`
}

// attachRules batch-fetches each promotion's discount rule (one query, not N+1) and pairs
// them up for serialization.
func (h *PromotionHandler) attachRules(ctx context.Context, promos []*ent.Promotion) []promotionWithRule {
	out := make([]promotionWithRule, len(promos))
	if len(promos) == 0 {
		return out
	}
	ids := make([]uuid.UUID, len(promos))
	for i, p := range promos {
		ids[i] = p.ID
	}
	rules, err := h.client.PromotionRule.Query().Where(promotionrule.PromotionIDIn(ids...)).All(ctx)
	if err != nil {
		h.log.Warn("attach promotion rules: query failed", zap.Error(err))
	}
	ruleByPromoID := make(map[uuid.UUID]*ent.PromotionRule, len(rules))
	for _, ru := range rules {
		ruleByPromoID[ru.PromotionID] = ru
	}
	for i, p := range promos {
		out[i] = promotionWithRule{Promotion: p, Rule: ruleByPromoID[p.ID]}
	}
	return out
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

	jsonOK(w, pagination.NewResponse(h.attachRules(r.Context(), promos), total, p))
}

// GetPromotion handles GET /{tenantID}/pos/promotions/{promoID} — single promotion with its
// discount rule, for the edit form.
func (h *PromotionHandler) GetPromotion(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	promoID, err := uuid.Parse(chi.URLParam(r, "promoID"))
	if err != nil {
		jsonError(w, "invalid promo_id", http.StatusBadRequest)
		return
	}
	promo, err := h.client.Promotion.Query().Where(promotion.ID(promoID), promotion.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "promotion not found", http.StatusNotFound)
		return
	}
	rule, _ := h.client.PromotionRule.Query().Where(promotionrule.PromotionID(promoID)).First(r.Context())
	jsonOK(w, promotionWithRule{Promotion: promo, Rule: rule})
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
	ScopeType     string   `json:"scope_type"`    // all | category | item — for BOGO, the "buy" scope
	ScopeIDs      []string `json:"scope_ids"`     // inventory category ids / skus
	DiscountType  string   `json:"discount_type"` // percentage | fixed_amount | fixed_price | bogo
	DiscountValue float64  `json:"discount_value"`
	MaxDiscount   *float64 `json:"max_discount"`
	MealPeriod    string   `json:"meal_period"` // optional: breakfast|am_break|lunch|pm_break|dinner (negotiated meal rate)
	// BOGO-only ("buy X get Y [at N% off]"): meaningful when DiscountType == "bogo" and
	// ScopeType == "item". Zero values fall back to sane defaults (1 buy, 1 get, 100% off)
	// in the rule builder below.
	BuyQuantity        int     `json:"buy_quantity"`
	GetQuantity        int     `json:"get_quantity"`
	GetDiscountPercent float64 `json:"get_discount_percent"`
	// GetScopeIDs enables CROSS-ITEM BOGO: SKUs eligible for the free/discounted "get" unit
	// when they are a DIFFERENT item from ScopeIDs — e.g. ScopeIDs=Large pizzas,
	// GetScopeIDs=Small pizzas ("buy one large, get one small free"). Empty = same-SKU BOGO.
	GetScopeIDs []string `json:"get_scope_ids"`
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

	// Persist the discount rule when provided (scope + discount type/value/cap). BOGO
	// carries its deal in buy/get quantities rather than DiscountValue, so it must trigger
	// this block on its own even when DiscountValue is left at 0.
	if input.DiscountValue > 0 || input.ScopeType != "" || input.DiscountType == "bogo" {
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
			SetGetScopeIds(input.GetScopeIDs).
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
		if discountType == "bogo" {
			buyQty, getQty, getPct := input.BuyQuantity, input.GetQuantity, input.GetDiscountPercent
			if buyQty <= 0 {
				buyQty = 1
			}
			if getQty <= 0 {
				getQty = 1
			}
			if getPct <= 0 {
				getPct = 100
			}
			rb = rb.SetBuyQuantity(buyQty).SetGetQuantity(getQty).SetGetDiscountPercent(getPct)
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
	// Embed each promo's rule (scope_ids/discount_type/buy-get) — the terminal's
	// add-to-cart waiter alert and the happy-hour "Active now" card both need to know
	// WHICH items are covered and WHAT the deal is, not just that something is live.
	jsonOK(w, h.attachRules(r.Context(), active))
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

// UpdatePromotion handles PATCH /{tenantID}/pos/promotions/{promoID} — edits an existing
// promotion (happy hour or otherwise) and upserts its discount rule. Reuses createPromoInput
// so the create and edit forms on the frontend can share one payload shape; every field is
// applied unconditionally (the edit form always submits the full, current state — there is
// no partial-patch semantics here, matching how the create endpoint already works).
func (h *PromotionHandler) UpdatePromotion(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	promoID, err := uuid.Parse(chi.URLParam(r, "promoID"))
	if err != nil {
		jsonError(w, "invalid promo_id", http.StatusBadRequest)
		return
	}
	existing, err := h.client.Promotion.Query().Where(promotion.ID(promoID), promotion.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "promotion not found", http.StatusNotFound)
		return
	}

	var input createPromoInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	upd := existing.Update().
		SetName(input.Name).
		SetAutoApply(input.AutoApply).
		SetDaysOfWeek(input.DaysOfWeek).
		SetWindowStart(input.WindowStart).
		SetWindowEnd(input.WindowEnd)
	if input.PromoKind != "" {
		upd = upd.SetPromoKind(promotion.PromoKind(input.PromoKind))
	}
	if input.StartAt != nil {
		upd = upd.SetStartAt(*input.StartAt)
	}
	if input.EndAt != nil {
		upd = upd.SetEndAt(*input.EndAt)
	} else {
		upd = upd.ClearEndAt()
	}
	if oid, perr := uuid.Parse(input.OutletID); perr == nil {
		upd = upd.SetOutletID(oid)
	} else {
		upd = upd.ClearOutletID()
	}
	promo, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("update promotion failed", zap.Error(err))
		jsonError(w, "failed to update promotion: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Upsert the discount rule (same field set as CreatePromotion — one rule per promotion).
	scopeType := input.ScopeType
	if scopeType == "" {
		scopeType = "all"
	}
	discountType := input.DiscountType
	if discountType == "" {
		discountType = "percentage"
	}
	rule, rerr := h.client.PromotionRule.Query().Where(promotionrule.PromotionID(promoID)).First(r.Context())
	var rb *ent.PromotionRuleUpdateOne
	if rerr == nil && rule != nil {
		rb = rule.Update().
			SetScopeType(promotionrule.ScopeType(scopeType)).
			SetScopeIds(input.ScopeIDs).
			SetGetScopeIds(input.GetScopeIDs).
			SetDiscountType(promotionrule.DiscountType(discountType)).
			SetDiscountValue(input.DiscountValue)
	}
	if rb == nil {
		created, cerr := h.client.PromotionRule.Create().
			SetPromotionID(promoID).
			SetRuleType("discount").
			SetScopeType(promotionrule.ScopeType(scopeType)).
			SetScopeIds(input.ScopeIDs).
			SetGetScopeIds(input.GetScopeIDs).
			SetDiscountType(promotionrule.DiscountType(discountType)).
			SetDiscountValue(input.DiscountValue).
			Save(r.Context())
		if cerr != nil {
			h.log.Error("update promotion: create rule failed", zap.Error(cerr))
		} else {
			rule = created
		}
	} else {
		if input.MaxDiscount != nil {
			rb = rb.SetMaxDiscount(*input.MaxDiscount)
		} else {
			rb = rb.ClearMaxDiscount()
		}
		if input.MealPeriod != "" {
			rb = rb.SetMealPeriod(promotionrule.MealPeriod(input.MealPeriod))
		} else {
			rb = rb.ClearMealPeriod()
		}
		if discountType == "bogo" {
			buyQty, getQty, getPct := input.BuyQuantity, input.GetQuantity, input.GetDiscountPercent
			if buyQty <= 0 {
				buyQty = 1
			}
			if getQty <= 0 {
				getQty = 1
			}
			if getPct <= 0 {
				getPct = 100
			}
			rb = rb.SetBuyQuantity(buyQty).SetGetQuantity(getQty).SetGetDiscountPercent(getPct)
		}
		if _, uerr := rb.Save(r.Context()); uerr != nil {
			h.log.Error("update promotion rule failed", zap.Error(uerr))
		}
	}

	rule, _ = h.client.PromotionRule.Query().Where(promotionrule.PromotionID(promoID)).First(r.Context())
	jsonOK(w, promotionWithRule{Promotion: promo, Rule: rule})
}

// DeletePromotion handles DELETE /{tenantID}/pos/promotions/{promoID} — soft-deletes by
// setting status=inactive rather than hard-deleting, since a PromotionApplication audit row
// may reference this promotion from a past sale (a hard delete would orphan that FK-less
// reference and break "which promo discounted this order" history).
func (h *PromotionHandler) DeletePromotion(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	promoID, err := uuid.Parse(chi.URLParam(r, "promoID"))
	if err != nil {
		jsonError(w, "invalid promo_id", http.StatusBadRequest)
		return
	}
	n, err := h.client.Promotion.Update().
		Where(promotion.ID(promoID), promotion.TenantID(tid)).
		SetStatus("inactive").
		Save(r.Context())
	if err != nil {
		h.log.Error("delete promotion failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		jsonError(w, "promotion not found", http.StatusNotFound)
		return
	}
	jsonOK(w, map[string]any{"deleted": true})
}
