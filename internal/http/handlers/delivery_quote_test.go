package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/platform/logistics"
)

type fakeQuoter struct {
	q   *logistics.Quote
	err error
}

func (f fakeQuoter) QuoteDelivery(context.Context, uuid.UUID, uuid.UUID, float64, float64, float64) (*logistics.Quote, error) {
	return f.q, f.err
}

func (f fakeQuoter) DeliveryAreas(context.Context, uuid.UUID, uuid.UUID) ([]logistics.DeliveryArea, error) {
	return nil, f.err
}

func newQuoteHandler(q DeliveryQuoter) *POSOrderHandler {
	h := &POSOrderHandler{log: zap.NewNop()}
	h.SetDeliveryQuoter(q)
	return h
}

func deliveryInput() *createOrderInput {
	return &createOrderInput{
		OrderSubtype: "delivery",
		Lines:        []createOrderLineInput{{Quantity: 1, UnitPrice: 500, TotalPrice: 500}},
		Metadata:     map[string]interface{}{"delivery_lat": 0.44899, "delivery_lng": 34.17005},
		Charges:      map[string]float64{"shipping": 999},
	}
}

func TestApplyDeliveryQuoteSetsQuotedFee(t *testing.T) {
	h := newQuoteHandler(fakeQuoter{q: &logistics.Quote{Serviceable: true, Fee: 150, Method: "zone"}})
	in := deliveryInput()
	rec := httptest.NewRecorder()
	quoted, ok := h.applyDeliveryQuote(rec, httptest.NewRequest(http.MethodPost, "/", nil), uuid.New(), uuid.New(), in)
	if !ok || !quoted {
		t.Fatalf("quoted=%v ok=%v", quoted, ok)
	}
	if in.Charges["shipping"] != 150 {
		t.Fatalf("typed charge must be replaced by the quote, got %v", in.Charges["shipping"])
	}
	if _, has := in.Metadata["delivery_quote"]; !has {
		t.Fatal("quote snapshot missing from metadata")
	}
}

func TestApplyDeliveryQuoteRejectsOutsideAreas(t *testing.T) {
	h := newQuoteHandler(fakeQuoter{q: &logistics.Quote{Serviceable: false, Reason: "outside_delivery_area"}})
	rec := httptest.NewRecorder()
	_, ok := h.applyDeliveryQuote(rec, httptest.NewRequest(http.MethodPost, "/", nil), uuid.New(), uuid.New(), deliveryInput())
	if ok || rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 and ok=false, got %d ok=%v", rec.Code, ok)
	}
}

func TestApplyDeliveryQuoteFallsBackWhenUnavailable(t *testing.T) {
	h := newQuoteHandler(fakeQuoter{err: errors.New("down")})
	in := deliveryInput()
	quoted, ok := h.applyDeliveryQuote(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), uuid.New(), uuid.New(), in)
	if !ok || quoted || in.Charges["shipping"] != 999 {
		t.Fatalf("logistics down must keep the typed charge for the approval gate: quoted=%v ok=%v charge=%v", quoted, ok, in.Charges["shipping"])
	}
}

func TestApplyDeliveryQuoteSkipsWithoutPin(t *testing.T) {
	h := newQuoteHandler(fakeQuoter{q: &logistics.Quote{Serviceable: true, Fee: 150}})
	in := deliveryInput()
	in.Metadata = map[string]interface{}{"delivery_address": "Bugengi"}
	quoted, ok := h.applyDeliveryQuote(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), uuid.New(), uuid.New(), in)
	if !ok || quoted {
		t.Fatalf("no pin must not quote: quoted=%v ok=%v", quoted, ok)
	}
}
