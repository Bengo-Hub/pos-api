package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent/promotion"
	"github.com/bengobox/pos-service/internal/ent/promotionrule"
	promotions "github.com/bengobox/pos-service/internal/modules/promotions"
)

// A status-only PATCH switches a promotion off without touching anything else. Before
// 2026-11-13 the update applied every field unconditionally, so {"status":"inactive"} failed on
// the empty name, and a body with only name and status cleared the outlet, the end date and
// the discount rule of a live deal.
func TestUpdatePromotion_PartialPatchKeepsTheRest(t *testing.T) {
	client := newPromoTestClient(t)
	tid := seedListTenant(t, client)
	h := NewPromotionHandler(zap.NewNop(), client, promotions.NewService(client, zap.NewNop()))
	outlet := uuid.New()
	end := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)

	call := func(method string, fn http.HandlerFunc, id string, body map[string]any) *httptest.ResponseRecorder {
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(method, "/promotions", bytes.NewReader(buf))
		ctx := httpware.WithTenantID(req.Context(), tid.String())
		if id != "" {
			rc := chi.NewRouteContext()
			rc.URLParams.Add("promoID", id)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, rc)
		}
		rec := httptest.NewRecorder()
		fn(rec, req.WithContext(ctx))
		return rec
	}

	rec := call(http.MethodPost, h.CreatePromotion, "", map[string]any{
		"name": "Shampoo 20%", "promo_kind": "auto", "auto_apply": true, "outlet_id": outlet.String(),
		"end_at": end, "scope_type": "item", "scope_ids": []string{"RTL-SHP-001"},
		"discount_type": "percentage", "discount_value": 20,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	p, err := client.Promotion.Query().Where(promotion.TenantID(tid)).Only(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	rec = call(http.MethodPatch, h.UpdatePromotion, p.ID.String(), map[string]any{"status": "inactive"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status-only patch: %d %s", rec.Code, rec.Body.String())
	}
	got, _ := client.Promotion.Get(context.Background(), p.ID)
	rule, _ := client.PromotionRule.Query().Where(promotionrule.PromotionID(p.ID)).Only(context.Background())
	if got.Status != "inactive" || got.Name != "Shampoo 20%" || !got.AutoApply ||
		got.OutletID == nil || *got.OutletID != outlet || got.EndAt == nil || !got.EndAt.Equal(end) {
		t.Fatalf("status-only patch changed other fields: %+v", got)
	}
	if rule.DiscountValue != 20 || string(rule.ScopeType) != "item" || len(rule.ScopeIds) != 1 {
		t.Fatalf("status-only patch changed the rule: %+v", rule)
	}

	// Back on, and a bad status or an empty name is refused.
	if rec = call(http.MethodPatch, h.UpdatePromotion, p.ID.String(), map[string]any{"status": "active"}); rec.Code != http.StatusOK {
		t.Fatalf("reactivate: %d", rec.Code)
	}
	if got, _ = client.Promotion.Get(context.Background(), p.ID); got.Status != "active" {
		t.Fatalf("status = %q, want active", got.Status)
	}
	if rec = call(http.MethodPatch, h.UpdatePromotion, p.ID.String(), map[string]any{"status": "paused"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad status: %d", rec.Code)
	}
	if rec = call(http.MethodPatch, h.UpdatePromotion, p.ID.String(), map[string]any{"name": " "}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name: %d", rec.Code)
	}
}
