package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Bengo-Hub/httpware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	promotions "github.com/bengobox/pos-service/internal/modules/promotions"
)

// newPromoTestClient opens a fresh in-memory sqlite ent client (sqlite3 driver registered once
// via held_items_test.go's init() in this same package).
func newPromoTestClient(t *testing.T) *ent.Client {
	t.Helper()
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:promolisttest_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func seedListTenant(t *testing.T, client *ent.Client) uuid.UUID {
	t.Helper()
	tenant, err := client.Tenant.Create().
		SetID(uuid.New()).
		SetName("Test Tenant").
		SetSlug("test-" + uuid.NewString()[:8]).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return tenant.ID
}

func seedListOutlet(t *testing.T, client *ent.Client, tid uuid.UUID) uuid.UUID {
	t.Helper()
	o, err := client.Outlet.Create().
		SetTenantID(tid).
		SetTenantSlug("codevertex-demo").
		SetCode("OUT-" + uuid.NewString()[:8]).
		SetName("Test Outlet").
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed outlet: %v", err)
	}
	return o.ID
}

func seedListPromo(t *testing.T, client *ent.Client, tid uuid.UUID, name string, outletID *uuid.UUID) {
	t.Helper()
	b := client.Promotion.Create().
		SetTenantID(tid).
		SetName(name).
		SetPromoCode(name).
		SetStatus("active")
	if outletID != nil {
		b = b.SetOutletID(*outletID)
	}
	if _, err := b.Save(context.Background()); err != nil {
		t.Fatalf("seed promo %q: %v", name, err)
	}
}

// ListPromotions must scope checkout-consumption reads (default/non-"all" status) to the calling
// outlet: tenant-wide promos (nil outlet_id) plus THIS outlet's promos, excluding any scoped to a
// different outlet. status=all (the admin management view) intentionally sees everything.
func TestListPromotions_OutletScoping(t *testing.T) {
	client := newPromoTestClient(t)
	tid := seedListTenant(t, client)
	outletA := seedListOutlet(t, client, tid)
	outletB := seedListOutlet(t, client, tid)

	seedListPromo(t, client, tid, "tenant-wide", nil)
	seedListPromo(t, client, tid, "outlet-a-only", &outletA)
	seedListPromo(t, client, tid, "outlet-b-only", &outletB)

	h := NewPromotionHandler(zap.NewNop(), client, promotions.NewService(client, zap.NewNop()))

	names := func(status, outletID string) []string {
		req := httptest.NewRequest(http.MethodGet, "/promotions?status="+status, nil)
		ctx := httpware.WithTenantID(req.Context(), tid.String())
		if outletID != "" {
			ctx = httpware.WithOutletID(ctx, outletID)
		}
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()
		h.ListPromotions(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Data []struct {
				Name string `json:"name"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
		}
		got := make([]string, 0, len(out.Data))
		for _, d := range out.Data {
			got = append(got, d.Name)
		}
		return got
	}

	atA := names("", outletA.String())
	if !promoListContainsAll(atA, "tenant-wide", "outlet-a-only") || promoListContains(atA, "outlet-b-only") {
		t.Fatalf("outlet A checkout view: expected tenant-wide+outlet-a-only only, got %v", atA)
	}

	all := names("all", "")
	if !promoListContainsAll(all, "tenant-wide", "outlet-a-only", "outlet-b-only") {
		t.Fatalf("status=all management view: expected every promo, got %v", all)
	}
}

func promoListContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func promoListContainsAll(list []string, ss ...string) bool {
	for _, s := range ss {
		if !promoListContains(list, s) {
			return false
		}
	}
	return true
}
