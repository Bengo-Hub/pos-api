package treasury

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The refund lines and the credit note only call must use the exact field names treasury-api
// decodes (arpa.RefundRequest, arpa.RefundLine and the pos/returns/credit-note handler).
func TestRaiseReturnCreditNoteContract(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"cn-1","invoice_number":"CN-000007","status":"sent"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key", 5*time.Second)
	cn, err := c.RaiseReturnCreditNote(context.Background(), "shop", ReturnCreditNoteRequest{
		ReferenceID: "ret-1", Reference: "RET-1", OriginalOrderID: "order-1", OutletID: "outlet-1",
		Lines: []RefundLine{{ItemSKU: "MUG", Description: "Mug", Quantity: 1, UnitPrice: 450, TaxCode: "VAT-16", TaxRate: 16}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/s2s/shop/pos/returns/credit-note" {
		t.Fatalf("called %s %s", gotMethod, gotPath)
	}
	if cn.Number != "CN-000007" || cn.ID != "cn-1" {
		t.Fatalf("response not decoded: %+v", cn)
	}
	for _, k := range []string{"reference_id", "reference", "original_order_id", "outlet_id", "lines"} {
		if _, ok := gotBody[k]; !ok {
			t.Errorf("body is missing %q: %v", k, gotBody)
		}
	}
	line := gotBody["lines"].([]any)[0].(map[string]any)
	for _, k := range []string{"item_sku", "description", "quantity", "unit_price", "tax_code", "tax_rate"} {
		if _, ok := line[k]; !ok {
			t.Errorf("line is missing %q: %v", k, line)
		}
	}
}

func TestRefundRequestCarriesFiscalLines(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"refund-1","status":"ok","amount":"100","currency":"KES"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key", 5*time.Second)
	_, err := c.CreateRefund(context.Background(), "shop", "ret-1", RefundRequest{
		SourceService: "pos", ReferenceID: "ret-1", ReferenceType: "pos_return", Amount: 100,
		OutletID: "outlet-1", OriginalOrderID: "order-1",
		Lines: []RefundLine{{Description: "Mug", Quantity: 1, UnitPrice: 86.2069, TaxRate: 16}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"outlet_id", "original_order_id", "lines"} {
		if _, ok := gotBody[k]; !ok {
			t.Errorf("refund body is missing %q: %v", k, gotBody)
		}
	}

	// A sale that was never fiscalised sends none of the three, so treasury keeps the old behaviour.
	gotBody = nil
	if _, err := c.CreateRefund(context.Background(), "shop", "ret-2", RefundRequest{SourceService: "pos", ReferenceID: "ret-2", Amount: 50}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"outlet_id", "original_order_id", "lines"} {
		if _, ok := gotBody[k]; ok {
			t.Errorf("a plain refund must not send %q: %v", k, gotBody)
		}
	}
}
