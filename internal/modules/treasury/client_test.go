package treasury

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

// PayHero is its own gateway: the POS gets one payhero flag with the rails treasury lists for the
// outlet's currency (asked with ?currency=), and M-Pesa stays off unless Daraja is on.
func TestGetPublicGatewaysMapsPayHero(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"gateways":["payhero","cod"],"payhero_methods":["mtn_momo","airtel_money","payhero_card"]}`))
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
	if !gw.PayHero || !gw.COD || !slices.Equal(gw.PayHeroMethods, []string{"mtn_momo", "airtel_money", "payhero_card"}) {
		t.Fatalf("flags = %+v, want payhero with its rails and cod", gw)
	}
	if gw.MPesa || gw.MPesaC2B || gw.Paystack {
		t.Fatalf("flags = %+v: gateways treasury did not list must stay off", gw)
	}
}

// M-Pesa is Daraja only, so it brings the C2B till matcher with it; PayHero alone never does.
func TestGetPublicGatewaysMpesaIsDaraja(t *testing.T) {
	cases := []struct {
		name, body string
		mpesa      bool
	}{
		{"payhero only", `{"gateways":["payhero"],"payhero_methods":["mpesa"]}`, false},
		{"daraja", `{"gateways":["mpesa"],"providers":{"mpesa":"daraja"}}`, true},
		{"both", `{"gateways":["payhero","mpesa"],"payhero_methods":["mpesa"]}`, true},
		{"none", `{"gateways":["cod","complimentary"]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			gw, err := NewClient(srv.URL, "key", 5*time.Second).GetPublicGateways(context.Background(), "shop", "KES")
			if err != nil {
				t.Fatal(err)
			}
			if gw.MPesa != tc.mpesa || gw.MPesaC2B != tc.mpesa {
				t.Fatalf("mpesa=%v c2b=%v, want %v", gw.MPesa, gw.MPesaC2B, tc.mpesa)
			}
		})
	}
}
