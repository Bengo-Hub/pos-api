package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/promotion"
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

	promos, err := query.Order(ent.Desc(promotion.FieldStartAt)).All(r.Context())
	if err != nil {
		h.log.Error("list promotions failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"data": promos, "total": len(promos)})
}

type createPromoInput struct {
	PromoCode  string     `json:"promoCode"`
	Name       string     `json:"name"`
	StartAt    *time.Time `json:"startAt"`
	EndAt      *time.Time `json:"endAt"`
	UsageLimit int        `json:"usageLimit"`
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
		SetPromoCode(input.PromoCode).
		SetStatus("active")

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

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, promo)
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
