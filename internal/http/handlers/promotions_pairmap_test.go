package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Bengo-Hub/httpware"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent/promotion"
	"github.com/bengobox/pos-service/internal/ent/promotionrule"
	promotions "github.com/bengobox/pos-service/internal/modules/promotions"
)

// TestCreatePromotion_AllowsSelfPairedGetPairMap is the end-to-end HTTP check for the Urban Loft
// "BURGER DAY" incident (2026-08-15): a get_pair_map entry where a buy SKU maps to itself (e.g.
// the tenant deliberately configures "buy 1 Urban Vege Burger, get 1 more Vege Burger free"
// alongside its cross-item pairs) is a legitimate deal shape and must be accepted — the actual
// bug was that calculateCorrespondingPairBOGO priced it wrong (zeroed the item outright), not
// that the shape itself was invalid. See TestCorrespondingPairBOGO_SelfPair* in the promotions
// package for the pricing-correctness coverage.
func TestCreatePromotion_AllowsSelfPairedGetPairMap(t *testing.T) {
	client := newPromoTestClient(t)
	tid := seedListTenant(t, client)
	h := NewPromotionHandler(zap.NewNop(), client, promotions.NewService(client, zap.NewNop()))

	body := map[string]any{
		"name":                 "BURGER DAY",
		"promo_kind":           "happy_hour",
		"auto_apply":           true,
		"discount_type":        "bogo",
		"scope_type":           "item",
		"buy_quantity":         1,
		"get_quantity":         1,
		"get_discount_percent": 100,
		"get_pair_map": map[string]string{
			"BUR001": "BUR005",
			"BUR005": "BUR005", // self-paired — must be allowed
			"BUR006": "BUR005",
		},
	}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/promotions", bytes.NewReader(buf))
	req = req.WithContext(httpware.WithTenantID(req.Context(), tid.String()))
	rec := httptest.NewRecorder()

	h.CreatePromotion(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a self-paired get_pair_map, got %d: %s", rec.Code, rec.Body.String())
	}

	promo, err := client.Promotion.Query().Where(promotion.TenantID(tid)).Only(req.Context())
	if err != nil {
		t.Fatalf("query created promotion: %v", err)
	}
	rule, err := client.PromotionRule.Query().Where(promotionrule.PromotionID(promo.ID)).Only(req.Context())
	if err != nil {
		t.Fatalf("query created rule: %v", err)
	}
	if got := rule.GetPairMap["BUR005"]; got != "BUR005" {
		t.Fatalf("expected the self-paired entry to persist unchanged, got %+v", rule.GetPairMap)
	}
}

// TestCreatePromotion_AllowsLegitimatePairedGetPairMap ensures the ordinary cross-item pairing
// shape (no self-pairs at all) still works.
func TestCreatePromotion_AllowsLegitimatePairedGetPairMap(t *testing.T) {
	client := newPromoTestClient(t)
	tid := seedListTenant(t, client)
	h := NewPromotionHandler(zap.NewNop(), client, promotions.NewService(client, zap.NewNop()))

	body := map[string]any{
		"name":                 "BURGER DAY",
		"promo_kind":           "happy_hour",
		"auto_apply":           true,
		"discount_type":        "bogo",
		"scope_type":           "item",
		"buy_quantity":         1,
		"get_quantity":         1,
		"get_discount_percent": 100,
		"get_pair_map": map[string]string{
			"BUR001": "BUR005",
			"BUR006": "BUR005",
		},
	}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/promotions", bytes.NewReader(buf))
	req = req.WithContext(httpware.WithTenantID(req.Context(), tid.String()))
	rec := httptest.NewRecorder()

	h.CreatePromotion(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a legitimate get_pair_map, got %d: %s", rec.Code, rec.Body.String())
	}
}
