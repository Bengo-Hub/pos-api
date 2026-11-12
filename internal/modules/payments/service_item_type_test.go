package payments

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// TestSignEtimsSync_SendsCatalogItemType is the regression test for GOODS items first sold at POS
// registering at KRA as a service (itemTyCd 3): the sync signing payload carried no item type, so
// treasury fell back to a service. The line's type now comes from the POS catalog projection
// (metadata["item_type"], cached by the inventory catalog sync).
func TestSignEtimsSync_SendsCatalogItemType(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	ctx := context.Background()
	tenantID := uuid.New()

	var got treasury.SignPOSSaleRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode sign request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"signed":false}`))
	}))
	defer srv.Close()
	svc.SetTreasuryClient(treasury.NewClient(srv.URL, "test-key", 5*time.Second))

	if _, err := client.Tenant.Create().
		SetID(tenantID).SetName("Item Type Tenant").SetSlug("item-type-tenant").
		Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	outlet, err := client.Outlet.Create().
		SetTenantID(tenantID).SetName("Main").SetCode("MAIN").SetTenantSlug("item-type-tenant").
		Save(ctx)
	if err != nil {
		t.Fatalf("seed outlet: %v", err)
	}
	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(outlet.ID).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(500).SetTaxTotal(0).SetTotalAmount(500).SetPaidTotal(500).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	for _, l := range []struct{ sku, name string }{{"SKU-GOODS", "Soap"}, {"SKU-UNSYNCED", "Unsynced"}} {
		if _, err := client.POSOrderLine.Create().
			SetOrderID(order.ID).SetCatalogItemID(uuid.New()).SetSku(l.sku).SetName(l.name).
			SetQuantity(1).SetUnitPrice(250).SetTotalPrice(250).
			Save(ctx); err != nil {
			t.Fatalf("seed line %s: %v", l.sku, err)
		}
	}
	if _, err := client.POSCatalogOverride.Create().
		SetTenantID(tenantID).SetInventorySku("SKU-GOODS").
		SetMetadata(map[string]any{"item_type": "GOODS"}).
		Save(ctx); err != nil {
		t.Fatalf("seed catalog projection: %v", err)
	}

	svc.signEtimsSync(ctx, order)

	if len(got.Items) != 2 {
		t.Fatalf("sign request items = %d, want 2 (request %+v)", len(got.Items), got)
	}
	types := map[string]string{}
	for _, it := range got.Items {
		types[it.SKU] = it.ItemType
	}
	if types["SKU-GOODS"] != "GOODS" {
		t.Errorf("SKU-GOODS item_type = %q, want GOODS", types["SKU-GOODS"])
	}
	if types["SKU-UNSYNCED"] != "" {
		t.Errorf("SKU-UNSYNCED item_type = %q, want blank (treasury keeps its service fallback)", types["SKU-UNSYNCED"])
	}
}

// TestSignPOSSaleItem_ItemTypeJSONTag pins the wire name treasury's tax.POSSaleItem reads.
func TestSignPOSSaleItem_ItemTypeJSONTag(t *testing.T) {
	b, err := json.Marshal(treasury.SignPOSSaleItem{SKU: "S1", ItemType: "GOODS"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["item_type"] != "GOODS" {
		t.Errorf("item_type = %v, want GOODS (payload %s)", m["item_type"], b)
	}
}
