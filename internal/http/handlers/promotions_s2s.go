package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/pagination"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/promotion"
)

// S2S discount endpoints — pos-api's Promotion + PromotionRule are the platform's
// DISCOUNT SOURCE OF TRUTH. Other services (inventory-api, ordering-backend,
// treasury-api, erp-api) must NOT define parallel discount/coupon entities; they
// list, create, and apply discounts against these endpoints (X-API-Key /
// INTERNAL_SERVICE_KEY, mounted under /api/v1/s2s/{tenant} — see router.go).
//
// The payload shapes are identical to the tenant-facing /pos/promotions handlers
// (createPromoInput / promotionWithRule), so the shared discount modal form used
// across frontends serializes one shape regardless of which service proxies it.

// S2SListDiscounts handles GET /api/v1/s2s/{tenant}/discounts?status=&kind=
// Returns the tenant's promotions with their discount rules attached. Defaults to
// active discounts; pass status=all for every status (management screens).
func (h *PromotionHandler) S2SListDiscounts(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant"))
	if err != nil {
		jsonError(w, "invalid tenant", http.StatusBadRequest)
		return
	}

	query := h.client.Promotion.Query().Where(promotion.TenantID(tid))
	switch status := r.URL.Query().Get("status"); status {
	case "all":
		// no status filter — management view
	case "":
		query = query.Where(promotion.Status("active"))
	default:
		query = query.Where(promotion.Status(status))
	}
	if kind := r.URL.Query().Get("kind"); kind != "" {
		query = query.Where(promotion.PromoKindEQ(promotion.PromoKind(kind)))
	}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		query = query.Where(promotion.NameContainsFold(q))
	}

	p := pagination.Parse(r)
	total, _ := query.Clone().Count(r.Context())
	promos, err := query.Order(ent.Desc(promotion.FieldStartAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("s2s list discounts failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(h.attachRules(r.Context(), promos), total, p))
}

// S2SCreateDiscount handles POST /api/v1/s2s/{tenant}/discounts — creates a discount
// in the source of truth on behalf of another service's UI (shared discount modal).
// Body: createPromoInput (same shape as the tenant-facing POST /pos/promotions).
func (h *PromotionHandler) S2SCreateDiscount(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant"))
	if err != nil {
		jsonError(w, "invalid tenant", http.StatusBadRequest)
		return
	}
	var input createPromoInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Name == "" {
		jsonError(w, "name required", http.StatusBadRequest)
		return
	}
	promo, err := h.createPromotionFromInput(r.Context(), tid, input)
	if err != nil {
		h.log.Error("s2s create discount failed", zap.Error(err))
		jsonError(w, "failed to create discount: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, promo)
}

// S2SApplyDiscount handles POST /api/v1/s2s/{tenant}/discounts/apply
// Body: {promoCode, amount}. Validates the code and returns the discount amount —
// the same evaluation the POS terminal uses, so a code behaves identically no
// matter which service applies it.
func (h *PromotionHandler) S2SApplyDiscount(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant"))
	if err != nil {
		jsonError(w, "invalid tenant", http.StatusBadRequest)
		return
	}
	var input struct {
		PromoCode string  `json:"promoCode"`
		Amount    float64 `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	result, err := h.promoSvc.ApplyPromoCode(r.Context(), tid, input.PromoCode, decimal.NewFromFloat(input.Amount))
	if err != nil {
		h.log.Error("s2s apply discount failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !result.Valid {
		jsonOK(w, map[string]any{"valid": false, "reason": result.Reason})
		return
	}
	jsonOK(w, map[string]any{
		"valid":          true,
		"promoCode":      result.PromoCode,
		"promoId":        result.PromoID,
		"discountAmount": result.DiscountAmount.StringFixed(2),
	})
}
