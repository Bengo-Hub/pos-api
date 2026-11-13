package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// A Charge to Room idempotency key over the limit is refused with 400 (re-review R-4), never
// silently dropped and treated as keyless. A missing room is refused the same way.
func TestCreatePaymentIntentRoomChargeKeyAndRoomChecked(t *testing.T) {
	h := NewPaymentHandler(zap.NewNop(), nil, nil, "", nil)
	tid := uuid.New()
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"key too long", map[string]any{"tenderMethod": "room_charge", "roomId": uuid.NewString(), "idempotencyKey": strings.Repeat("k", maxRoomChargeKeyLen+1)}, "idempotencyKey"},
		{"no room", map[string]any{"tenderMethod": "room_charge"}, "choose the room"},
	} {
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(tc.body)
		req := httptest.NewRequest(http.MethodPost, "/", &buf)
		req = req.WithContext(withTestTenant(req.Context(), tid))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("tenantID", "hotel")
		rctx.URLParams.Add("orderID", uuid.NewString())
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.CreatePaymentIntent(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: status %d body %s, want 400 mentioning %q", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}
}
