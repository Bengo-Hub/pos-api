package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
)

func seedSale(t *testing.T, client *ent.Client, tid uuid.UUID, status string, lines ...[3]any) {
	t.Helper()
	o := seedOrder(t, client, tid, uuid.New(), status)
	for _, l := range lines {
		if _, err := client.POSOrderLine.Create().
			SetOrderID(o.ID).
			SetCatalogItemID(uuid.New()).
			SetSku(l[0].(string)).
			SetName(l[0].(string) + " name").
			SetQuantity(l[1].(float64)).
			SetUnitPrice(1).
			SetTotalPrice(l[2].(float64)).
			Save(context.Background()); err != nil {
			t.Fatalf("seed line: %v", err)
		}
	}
}

// The S2S sales-by-sku report sums in the database: totals per SKU across orders, only completed
// orders of the tenant, and optional order and limit for best-seller callers.
func TestS2SSalesBySKU_GroupsInDatabase(t *testing.T) {
	client := openSQLiteTestClient(t, "salesbysku")
	t.Cleanup(func() { _ = client.Close() })
	tid, other := uuid.New(), uuid.New()

	seedSale(t, client, tid, "completed", [3]any{"A", 2.0, 200.0}, [3]any{"B", 1.0, 900.0})
	seedSale(t, client, tid, "completed", [3]any{"A", 3.0, 300.0}, [3]any{"C", 10.0, 100.0})
	seedSale(t, client, tid, "voided", [3]any{"C", 99.0, 9900.0})
	seedSale(t, client, other, "completed", [3]any{"A", 50.0, 5000.0})

	h := NewReportsHandler(zap.NewNop(), client)
	call := func(query string) []skuSales {
		req := httptest.NewRequest(http.MethodGet, "/?"+query, nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("tenant", tid.String())
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.S2SSalesBySKU(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Data []skuSales `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Data
	}

	all := call("")
	if len(all) != 3 {
		t.Fatalf("want 3 SKUs (A, B, C), got %+v", all)
	}
	by := map[string]skuSales{}
	for _, r := range all {
		by[r.SKU] = r
	}
	if a := by["A"]; a.QuantitySold != 5 || a.Revenue != 500 || a.Name != "A name" {
		t.Fatalf("A = %+v, want 5 units and 500 revenue", a)
	}
	if c := by["C"]; c.QuantitySold != 10 || c.Revenue != 100 {
		t.Fatalf("C = %+v: a voided order was counted", c)
	}

	top := call("order=quantity&limit=2")
	if len(top) != 2 || top[0].SKU != "C" || top[1].SKU != "A" {
		t.Fatalf("top 2 by quantity = %+v, want C then A", top)
	}
	rev := call("order=revenue&limit=1")
	if len(rev) != 1 || rev[0].SKU != "B" {
		t.Fatalf("top by revenue = %+v, want B", rev)
	}
}
