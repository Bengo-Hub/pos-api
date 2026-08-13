package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

// seedCompletedSaleWithCustomer is seedCompletedSale's sibling for the group_by=customer test —
// POS orders have no direct customer FK, only the phone/name captured at sale time.
func seedCompletedSaleWithCustomer(t *testing.T, client *ent.Client, tid, outletID uuid.UUID, sku, name string, revenue float64, phone, customerName string) {
	t.Helper()
	create := client.POSOrder.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetDeviceID(uuid.New()).
		SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).
		SetStatus("completed").
		SetSubtotal(revenue).
		SetTaxTotal(0).
		SetTotalAmount(revenue)
	if phone != "" {
		create = create.SetCustomerPhone(phone)
	}
	if customerName != "" {
		create = create.SetCustomerName(customerName)
	}
	o, err := create.Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if _, err := client.POSOrderLine.Create().
		SetOrderID(o.ID).
		SetCatalogItemID(uuid.New()).
		SetSku(sku).
		SetName(name).
		SetQuantity(1).
		SetUnitPrice(revenue).
		SetTotalPrice(revenue).
		Save(context.Background()); err != nil {
		t.Fatalf("seed order line: %v", err)
	}
}

// TestMostProfitableItems_GroupByOutlet_AggregatesOrderLevel guards the ORDER-level grouping path
// (outlet/staff/day/customer) added alongside the pre-existing per-SKU manufacturer/category/brand
// rollup — a genuinely different code path (aggregates off `orders` directly, not the per-sku
// `buckets`), so it needs its own coverage rather than assuming the item-level tests cover it.
func TestMostProfitableItems_GroupByOutlet_AggregatesOrderLevel(t *testing.T) {
	h, client := newReportsTestHandler(t)
	tid := uuid.New()
	outletA, outletB := uuid.New(), uuid.New()

	seedCompletedSale(t, client, tid, outletA, "SKU-A", "Widget A", 1000)
	seedCompletedSale(t, client, tid, outletB, "SKU-B", "Widget B", 500)
	if _, err := client.POSCatalogOverride.Create().
		SetTenantID(tid).SetInventorySku("SKU-A").
		SetMetadata(map[string]any{"cost_price": 400.0}).Save(context.Background()); err != nil {
		t.Fatalf("seed cost cache A: %v", err)
	}
	if _, err := client.POSCatalogOverride.Create().
		SetTenantID(tid).SetInventorySku("SKU-B").
		SetMetadata(map[string]any{"cost_price": 100.0}).Save(context.Background()); err != nil {
		t.Fatalf("seed cost cache B: %v", err)
	}

	req := reportsRequest(t, tid, nil, "group_by=outlet&from=2020-01-01&to=2030-01-01")
	rec := httptest.NewRecorder()
	h.MostProfitableItems(rec, req)

	if rec.Code != 200 {
		t.Fatalf("MostProfitableItems: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Groups []struct {
			Group     string  `json:"group"`
			Revenue   float64 `json:"revenue"`
			Profit    float64 `json:"profit"`
			MarginPct float64 `json:"margin_pct"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(body.Groups) != 2 {
		t.Fatalf("groups = %d, want 2 (one per outlet), body = %s", len(body.Groups), rec.Body.String())
	}
	byOutlet := map[string]float64{}
	for _, g := range body.Groups {
		byOutlet[g.Group] = g.Profit
	}
	if byOutlet[outletA.String()] != 600 {
		t.Errorf("outlet A profit = %v, want 600 (1000 revenue - 400 cost)", byOutlet[outletA.String()])
	}
	if byOutlet[outletB.String()] != 400 {
		t.Errorf("outlet B profit = %v, want 400 (500 revenue - 100 cost)", byOutlet[outletB.String()])
	}
}

// TestMostProfitableItems_GroupByCustomer_WalkInsBucketUnderUnknown guards the walk-in fallback
// and the CustomerName display-name resolution (POSOrder has no customer FK — only phone/name
// captured at sale time).
func TestMostProfitableItems_GroupByCustomer_WalkInsBucketUnderUnknown(t *testing.T) {
	h, client := newReportsTestHandler(t)
	tid := uuid.New()
	outletID := uuid.New()

	seedCompletedSaleWithCustomer(t, client, tid, outletID, "SKU-1", "Widget", 1000, "+254700000001", "Jane Doe")
	seedCompletedSaleWithCustomer(t, client, tid, outletID, "SKU-2", "Gadget", 2000, "", "") // walk-in

	req := reportsRequest(t, tid, &outletID, "group_by=customer&from=2020-01-01&to=2030-01-01")
	rec := httptest.NewRecorder()
	h.MostProfitableItems(rec, req)

	if rec.Code != 200 {
		t.Fatalf("MostProfitableItems: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Groups []struct {
			Group   string  `json:"group"`
			Revenue float64 `json:"revenue"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	var sawJane, sawUnknown bool
	for _, g := range body.Groups {
		switch g.Group {
		case "Jane Doe":
			sawJane = true
			if g.Revenue != 1000 {
				t.Errorf("Jane Doe revenue = %v, want 1000", g.Revenue)
			}
		case "Unknown":
			sawUnknown = true
			if g.Revenue != 2000 {
				t.Errorf("Unknown (walk-in) revenue = %v, want 2000", g.Revenue)
			}
		}
	}
	if !sawJane {
		t.Errorf("expected a group displayed as %q (from CustomerName), got groups = %+v", "Jane Doe", body.Groups)
	}
	if !sawUnknown {
		t.Errorf("expected walk-in orders to bucket under %q, got groups = %+v", "Unknown", body.Groups)
	}
}

// TestMostProfitableItems_GroupByBrand_RollsUpSameAsManufacturerCategory guards the new brand_name
// cache field end-to-end through the SAME rollup path manufacturer/category already use.
func TestMostProfitableItems_GroupByBrand_RollsUpSameAsManufacturerCategory(t *testing.T) {
	h, client := newReportsTestHandler(t)
	tid := uuid.New()
	outletID := uuid.New()

	seedCompletedSale(t, client, tid, outletID, "SKU-1", "Phone A", 1000)
	seedCompletedSale(t, client, tid, outletID, "SKU-2", "Phone B", 2000)
	if _, err := client.POSCatalogOverride.Create().
		SetTenantID(tid).SetInventorySku("SKU-1").
		SetMetadata(map[string]any{"cost_price": 400.0, "brand_name": "Samsung"}).
		Save(context.Background()); err != nil {
		t.Fatalf("seed cost cache 1: %v", err)
	}
	if _, err := client.POSCatalogOverride.Create().
		SetTenantID(tid).SetInventorySku("SKU-2").
		SetMetadata(map[string]any{"cost_price": 800.0, "brand_name": "Samsung"}).
		Save(context.Background()); err != nil {
		t.Fatalf("seed cost cache 2: %v", err)
	}

	req := reportsRequest(t, tid, &outletID, "group_by=brand&from=2020-01-01&to=2030-01-01")
	rec := httptest.NewRecorder()
	h.MostProfitableItems(rec, req)

	if rec.Code != 200 {
		t.Fatalf("MostProfitableItems: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Groups []struct {
			Group   string  `json:"group"`
			Revenue float64 `json:"revenue"`
			Profit  float64 `json:"profit"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(body.Groups) != 1 || body.Groups[0].Group != "Samsung" {
		t.Fatalf("groups = %+v, want a single %q group", body.Groups, "Samsung")
	}
	if body.Groups[0].Revenue != 3000 {
		t.Errorf("Samsung revenue = %v, want 3000", body.Groups[0].Revenue)
	}
	if body.Groups[0].Profit != 1800 {
		t.Errorf("Samsung profit = %v, want 1800 (3000 revenue - 1200 cost)", body.Groups[0].Profit)
	}
}
