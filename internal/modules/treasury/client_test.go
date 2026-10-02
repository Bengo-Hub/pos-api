package treasury

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The POS shows what treasury's pay page lists for the tenant, including the PayHero rails for the
// outlet's currency (asked with ?currency=).
func TestGetPublicGatewaysMapsPayHeroMethods(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"gateways":["mpesa","mtn_momo","airtel_money","payhero_momo","payhero_card","payhero_bank","payhero_offline"]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key", 5*time.Second)
	gw, err := c.GetPublicGateways(context.Background(), "shop", "ugx")
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "currency=UGX" {
		t.Fatalf("query = %q, want currency=UGX", gotQuery)
	}
	if !gw.MPesa || !gw.MTNMoMo || !gw.AirtelMoney || !gw.MobileMoney || !gw.PayHeroCard || !gw.PayHeroBank || !gw.PayHeroOffline {
		t.Fatalf("flags = %+v, want every listed rail on", gw)
	}
	if gw.Paystack || gw.BankTransfer {
		t.Fatalf("flags = %+v: rails treasury did not list must stay off", gw)
	}
}
