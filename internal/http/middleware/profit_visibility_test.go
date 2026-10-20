package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func TestStripProfitFields(t *testing.T) {
	in := `{"items":[{"sku":"A","revenue":100,"profit":40.5,"margin_pct":40.5,"unit_cost":59.5,"nested":{"gross_profit":1,"keep":2}}],"gross_margin_pct":3,"total":9007199254740993}`
	var out map[string]any
	if err := json.Unmarshal(stripProfitFields([]byte(in)), &out); err != nil {
		t.Fatal(err)
	}
	item := out["items"].([]any)[0].(map[string]any)
	for _, k := range []string{"profit", "margin_pct", "unit_cost"} {
		if _, ok := item[k]; ok {
			t.Errorf("%s survived", k)
		}
	}
	if item["revenue"] != float64(100) || item["sku"] != "A" {
		t.Errorf("non-profit fields changed: %v", item)
	}
	nested := item["nested"].(map[string]any)
	if _, ok := nested["gross_profit"]; ok || nested["keep"] != float64(2) {
		t.Errorf("nested not stripped correctly: %v", nested)
	}
	if _, ok := out["gross_margin_pct"]; ok {
		t.Error("top-level gross_margin_pct survived")
	}
	if got := string(stripProfitFields([]byte("not json"))); got != "not json" {
		t.Errorf("invalid JSON must pass through, got %q", got)
	}
	// Large integers keep their exact digits (UseNumber), not float rounding.
	if got := string(stripProfitFields([]byte(`{"total":9007199254740993}`))); got != `{"total":9007199254740993}` {
		t.Errorf("number mangled: %s", got)
	}
}

func TestHideProfitForCashier(t *testing.T) {
	client := newOutletContextTestClient(t)
	defer client.Close()
	ctx := context.Background()

	tenantID := uuid.New()
	if _, err := client.Tenant.Create().SetID(tenantID).SetName("Acme").SetSlug("acme-" + uuid.NewString()).Save(ctx); err != nil {
		t.Fatal(err)
	}
	mkOutlet := func(code string, hide *bool) uuid.UUID {
		o, err := client.Outlet.Create().SetTenantID(tenantID).SetTenantSlug("acme").SetCode(code).SetName(code).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		meta := map[string]any{}
		if hide != nil {
			meta[MetaKeyHideProfitMarginForCashier] = *hide
		}
		if _, err := client.OutletSetting.Create().SetOutletID(o.ID).SetMetadata(meta).Save(ctx); err != nil {
			t.Fatal(err)
		}
		return o.ID
	}
	yes, no := true, false
	hiddenOutlet, shownOutlet, unsetOutlet := mkOutlet("H", &yes), mkOutlet("S", &no), mkOutlet("U", nil)

	body := `{"id":"1","total":500,"profit":120,"margin_pct":24}`
	call := func(path string, outlet uuid.UUID, perms ...string) *httptest.ResponseRecorder {
		h := HideProfitForCashier(client, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if ProfitHidden(r.Context()) {
				w.Header().Set("X-Hidden", "1")
			}
			_, _ = w.Write([]byte(body))
		}))
		req := httptest.NewRequest(http.MethodGet, path, nil)
		c := req.Context()
		c = authclient.ContextWithClaims(c, &authclient.Claims{
			TenantID: tenantID.String(), Permissions: perms,
			RegisteredClaims: jwt.RegisteredClaims{Subject: uuid.NewString()},
		})
		c = context.WithValue(c, outletContextKey{}, &OutletContext{ID: outlet})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req.WithContext(c))
		return rec
	}
	has := func(rec *httptest.ResponseRecorder, key string) bool {
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		_, ok := m[key]
		return ok
	}

	t.Run("cashier on a hidden outlet loses profit and margin only", func(t *testing.T) {
		rec := call("/t/pos/orders", hiddenOutlet, "pos.orders.view_own")
		if has(rec, "profit") || has(rec, "margin_pct") || !has(rec, "total") || rec.Header().Get("X-Hidden") != "1" {
			t.Fatalf("unexpected body %s", rec.Body.String())
		}
	})
	t.Run("manager on a hidden outlet sees everything", func(t *testing.T) {
		rec := call("/t/pos/orders", hiddenOutlet, "pos.orders.manage")
		if !has(rec, "profit") || !has(rec, "margin_pct") {
			t.Fatalf("manager lost profit: %s", rec.Body.String())
		}
	})
	t.Run("cashier on an outlet that did not opt in sees everything", func(t *testing.T) {
		for _, o := range []uuid.UUID{shownOutlet, unsetOutlet} {
			if rec := call("/t/pos/orders", o, "pos.orders.view_own"); !has(rec, "profit") {
				t.Fatalf("outlet %s hid profit without the setting: %s", o, rec.Body.String())
			}
		}
	})
	t.Run("profit-only reports are refused for a hidden cashier", func(t *testing.T) {
		for _, p := range profitOnlyPaths {
			if rec := call(fmt.Sprintf("/t/pos%s", p), hiddenOutlet, "pos.reports.view"); rec.Code != http.StatusForbidden {
				t.Errorf("%s answered %d, want 403", p, rec.Code)
			}
			if rec := call(fmt.Sprintf("/t/pos%s", p), hiddenOutlet, "pos.orders.manage"); rec.Code != http.StatusOK {
				t.Errorf("%s answered %d for a manager, want 200", p, rec.Code)
			}
		}
	})
}
