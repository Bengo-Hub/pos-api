package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	promotions "github.com/bengobox/pos-service/internal/modules/promotions"
)

func seedStorefrontPromo(t *testing.T, client *ent.Client, tid uuid.UUID, name string, banner map[string]any, start time.Time, end *time.Time) {
	t.Helper()
	b := client.Promotion.Create().
		SetTenantID(tid).
		SetName(name).
		SetPromoCode(name).
		SetStatus("active").
		SetStartAt(start)
	if end != nil {
		b = b.SetEndAt(*end)
	}
	if banner != nil {
		b = b.SetMetadata(map[string]any{"banner": banner})
	}
	if _, err := b.Save(context.Background()); err != nil {
		t.Fatalf("seed promo %q: %v", name, err)
	}
}

// S2SListStorefrontDiscounts returns only the live, storefront-flagged promotions of the tenants
// asked for (one call for many tenants), soonest ending first, each carrying its tenant_id.
func TestS2SListStorefrontDiscounts(t *testing.T) {
	client := newPromoTestClient(t)
	a, b, other := seedListTenant(t, client), seedListTenant(t, client), seedListTenant(t, client)
	now := time.Now()
	soon, later, past := now.Add(2*time.Hour), now.Add(48*time.Hour), now.Add(-time.Hour)

	seedStorefrontPromo(t, client, a, "a-flash", map[string]any{"is_flash_sale": true}, now.Add(-time.Hour), &soon)
	seedStorefrontPromo(t, client, a, "a-banner", map[string]any{"show_on_storefront": true}, now.Add(-time.Hour), &later)
	seedStorefrontPromo(t, client, a, "a-plain", nil, now.Add(-time.Hour), &later)
	seedStorefrontPromo(t, client, b, "b-ended", map[string]any{"is_flash_sale": true}, now.Add(-48*time.Hour), &past)
	seedStorefrontPromo(t, client, b, "b-future", map[string]any{"is_flash_sale": true}, now.Add(time.Hour), &later)
	seedStorefrontPromo(t, client, other, "other-flash", map[string]any{"is_flash_sale": true}, now.Add(-time.Hour), &soon)

	h := NewPromotionHandler(zap.NewNop(), client, promotions.NewService(client, zap.NewNop()))
	req := httptest.NewRequest(http.MethodGet, "/s2s/storefront/discounts?tenant_ids="+a.String()+",bad,"+b.String(), nil)
	rec := httptest.NewRecorder()
	h.S2SListStorefrontDiscounts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []struct {
			Name     string `json:"name"`
			TenantID string `json:"tenant_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	names := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		names = append(names, d.Name)
		if d.TenantID != a.String() {
			t.Fatalf("%s carries tenant %s", d.Name, d.TenantID)
		}
	}
	if strings.Join(names, ",") != "a-flash,a-banner" {
		t.Fatalf("want live flagged promos soonest ending first [a-flash a-banner], got %v", names)
	}

	// No usable tenant id: an empty list, not an error.
	rec = httptest.NewRecorder()
	h.S2SListStorefrontDiscounts(rec, httptest.NewRequest(http.MethodGet, "/s2s/storefront/discounts", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("empty request: %d %s", rec.Code, rec.Body.String())
	}
}

// S2SListBanners carries is_flash_sale and end_at, so the storefront can show a real countdown.
func TestS2SListBannersCarriesFlashSaleEnd(t *testing.T) {
	client := newPromoTestClient(t)
	tid := seedListTenant(t, client)
	// Local time like the other seeds: sqlite compares timestamps as text, so mixing offsets
	// would fail the end_at >= now window check here (Postgres compares real timestamps).
	end := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	seedStorefrontPromo(t, client, tid, "flash", map[string]any{"show_on_storefront": true, "is_flash_sale": true}, time.Now().Add(-time.Hour), &end)

	h := NewPromotionHandler(zap.NewNop(), client, promotions.NewService(client, zap.NewNop()))
	req := httptest.NewRequest(http.MethodGet, "/s2s/"+tid.String()+"/discounts/banners", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("tenant", tid.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.S2SListBanners(rec, req)

	var got []struct {
		IsFlashSale bool       `json:"is_flash_sale"`
		EndAt       *time.Time `json:"end_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(got) != 1 || !got[0].IsFlashSale || got[0].EndAt == nil || !got[0].EndAt.Equal(end) {
		t.Fatalf("want one flash-sale banner ending %s, got %s", end, rec.Body.String())
	}
}
