package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func serveWithOutlet(h http.Handler, useCase string) int {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if useCase != "-" {
		req = req.WithContext(context.WithValue(req.Context(), outletContextKey{},
			&OutletContext{ID: uuid.New(), UseCase: useCase}))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

func TestRequireUseCaseNormalizesRawValues(t *testing.T) {
	h := RequireUseCase("services")(okHandler)
	cases := map[string]int{
		"services":       http.StatusOK,
		"Services":       http.StatusOK,
		"salon":          http.StatusOK, // normalizes to services
		"spa & wellness": http.StatusOK,
		"retail":         http.StatusForbidden,
		"hospitality":    http.StatusForbidden,
		"":               http.StatusOK, // no use case on the outlet: RBAC decides
		"-":              http.StatusOK, // no outlet context at all
	}
	for uc, want := range cases {
		if got := serveWithOutlet(h, uc); got != want {
			t.Errorf("use_case %q: status %d, want %d", uc, got, want)
		}
	}

	hosp := RequireUseCase("hospitality")(okHandler)
	if got := serveWithOutlet(hosp, "hotel"); got != http.StatusOK {
		t.Errorf("hotel must reach hospitality routes, got %d", got)
	}
}

func TestGateUnlessUseCase(t *testing.T) {
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusPaymentRequired) })
	}
	h := GateUnlessUseCase(deny, "services")(okHandler)
	if got := serveWithOutlet(h, "services"); got != http.StatusOK {
		t.Errorf("services must skip the gate, got %d", got)
	}
	if got := serveWithOutlet(h, "salon"); got != http.StatusOK {
		t.Errorf("salon normalizes to services and must skip the gate, got %d", got)
	}
	if got := serveWithOutlet(h, "hospitality"); got != http.StatusPaymentRequired {
		t.Errorf("hospitality must hit the gate, got %d", got)
	}
	if got := serveWithOutlet(h, "-"); got != http.StatusPaymentRequired {
		t.Errorf("no outlet context must hit the gate, got %d", got)
	}
}
