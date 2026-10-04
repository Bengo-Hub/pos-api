package subscriptions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

// fakeCheckAPI answers /usage/check like subscriptions-api: 402 with the limit body when the
// tenant is at its limit, 200 otherwise. It never counts anything.
func fakeCheckAPI(t *testing.T, used, limit float64) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var calls []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/usage/check" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["tenant"] = r.Header.Get("X-Tenant-ID")
		calls = append(calls, body)
		if used+body["value"].(float64) > limit {
			w.WriteHeader(http.StatusPaymentRequired)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "usage_limit_exceeded", "limit": limit, "used": used})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestCheckUsageAllowsUnderLimit(t *testing.T) {
	srv, calls := fakeCheckAPI(t, 299, 300)
	c := NewClient(Config{ServiceURL: srv.URL, APIKey: "test-key"}, zap.NewNop())
	if dec := c.CheckUsage(context.Background(), "tenant-1", MetricOrders, 1); !dec.Allowed {
		t.Fatalf("299/300 must allow one more sale: %+v", dec)
	}
	if got := (*calls)[0]; got["metric_type"] != "orders" || got["tenant"] != "tenant-1" {
		t.Fatalf("request %+v", got)
	}
}

func TestCheckUsageBlocksAtLimit(t *testing.T) {
	srv, _ := fakeCheckAPI(t, 300, 300)
	c := NewClient(Config{ServiceURL: srv.URL, APIKey: "test-key"}, zap.NewNop())
	dec := c.CheckUsage(context.Background(), "tenant-1", MetricOrders, 1)
	if dec.Allowed || dec.Status != http.StatusPaymentRequired || dec.Body["code"] != "usage_limit_exceeded" {
		t.Fatalf("300/300 must block with the limit body: %+v", dec)
	}
}

func TestCheckUsageFailsOpen(t *testing.T) {
	if dec := NewClient(Config{}, zap.NewNop()).CheckUsage(context.Background(), "t", MetricOrders, 1); !dec.Allowed {
		t.Fatal("unconfigured client must fail open")
	}
	down := NewClient(Config{ServiceURL: "http://127.0.0.1:1", APIKey: "k"}, zap.NewNop())
	if dec := down.CheckUsage(context.Background(), "t", MetricOrders, 1); !dec.Allowed {
		t.Fatal("unreachable subscriptions-api must fail open")
	}
}
