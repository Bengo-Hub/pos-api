package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Bengo-Hub/httpware"
	"go.uber.org/zap"

	promotions "github.com/bengobox/pos-service/internal/modules/promotions"
)

// TestValidateGetPairMap_RejectsSelfPair is a pure regression test for the exact corruption found
// on the Urban Loft "BURGER DAY" promotion in production (2026-08-15): a get_pair_map entry whose
// buy SKU equals its own get SKU. That shape earns the item a free credit for itself the moment
// it's bought (calculateCorrespondingPairBOGO has no same-item check), zeroing its own price.
func TestValidateGetPairMap_RejectsSelfPair(t *testing.T) {
	err := validateGetPairMap(map[string]string{
		"BUR001": "BUR005",
		"BUR005": "BUR005", // self-paired — must be rejected
		"BUR006": "BUR005",
	})
	if err == nil {
		t.Fatal("expected an error for a self-paired get_pair_map entry, got nil")
	}
}

func TestValidateGetPairMap_AllowsLegitimateMap(t *testing.T) {
	err := validateGetPairMap(map[string]string{
		"BUR001": "BUR005",
		"BUR002": "BUR005",
		"BUR006": "BUR005",
	})
	if err != nil {
		t.Fatalf("expected no error for a legitimate map, got %v", err)
	}
}

func TestValidateGetPairMap_EmptyMapOK(t *testing.T) {
	if err := validateGetPairMap(nil); err != nil {
		t.Fatalf("expected no error for an empty/nil map, got %v", err)
	}
}

// TestCreatePromotion_RejectsSelfPairedGetPairMap is the end-to-end HTTP check: POSTing a BOGO
// promotion whose get_pair_map self-pairs an item must 400 and must NOT persist a rule at all —
// the exact request shape that produced the Urban Loft bug must now be refused at the door.
func TestCreatePromotion_RejectsSelfPairedGetPairMap(t *testing.T) {
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
			"BUR005": "BUR005",
			"BUR006": "BUR005",
		},
	}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/promotions", bytes.NewReader(buf))
	req = req.WithContext(httpware.WithTenantID(req.Context(), tid.String()))
	rec := httptest.NewRecorder()

	h.CreatePromotion(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a self-paired get_pair_map, got %d: %s", rec.Code, rec.Body.String())
	}
	count, err := client.Promotion.Query().Count(req.Context())
	if err != nil {
		t.Fatalf("count promotions: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no promotion persisted after a rejected create, found %d", count)
	}
}

// TestCreatePromotion_AllowsLegitimatePairedGetPairMap ensures the new validation doesn't
// collateral-damage the normal, valid cross-item pairing shape.
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
