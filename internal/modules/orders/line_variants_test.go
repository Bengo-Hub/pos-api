package orders

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/inventory"
)

type stubVariantLookup struct {
	found map[string]inventory.VariantChild
	err   error
	calls int
}

func (s *stubVariantLookup) LookupVariantChildren(_ context.Context, _ string, skus []string) (map[string]inventory.VariantChild, error) {
	s.calls++
	out := map[string]inventory.VariantChild{}
	for _, sku := range skus {
		if v, ok := s.found[sku]; ok {
			out[sku] = v
		}
	}
	return out, s.err
}

func TestStampLineVariants(t *testing.T) {
	childID := uuid.New()
	lookup := &stubVariantLookup{found: map[string]inventory.VariantChild{
		"TSHIRT-RED-M": {ID: childID.String(), SKU: "TSHIRT-RED-M", Name: "T-shirt, Red / M",
			Attributes: map[string]string{"size": "M", "colour": "Red"}, ParentID: "p1", ParentSKU: "TSHIRT", ParentName: "T-shirt"},
	}}
	svc := &Service{log: zap.NewNop()}
	svc.SetVariantResolver(NewVariantResolver(lookup, nil, zap.NewNop()))

	lines := []OrderLineInput{
		{SKU: "TSHIRT-RED-M"},
		{SKU: "CAP", Metadata: map[string]any{LineVariantKey: map[string]any{"id": "forged"}, "note": "keep"}},
	}
	svc.StampLineVariants(context.Background(), uuid.New(), lines)

	snap, ok := lines[0].Metadata[LineVariantKey].(map[string]any)
	if !ok {
		t.Fatal("variant line must carry the server snapshot")
	}
	if snap["sku"] != "TSHIRT-RED-M" || snap["parent_sku"] != "TSHIRT" || snap["name"] != "T-shirt, Red / M" {
		t.Fatalf("snapshot = %v", snap)
	}
	if lines[0].CatalogItemID != childID {
		t.Fatal("a line with no catalog item id gets the child item id")
	}
	if _, forged := lines[1].Metadata[LineVariantKey]; forged {
		t.Fatal("a client-sent variant on a plain line must be dropped")
	}
	if lines[1].Metadata["note"] != "keep" {
		t.Fatal("other metadata must stay")
	}
	if lookup.calls != 1 {
		t.Fatalf("one lookup for the whole order, got %d", lookup.calls)
	}
}

func TestStampLineVariantsLookupFailureKeepsSale(t *testing.T) {
	svc := &Service{log: zap.NewNop()}
	svc.SetVariantResolver(NewVariantResolver(&stubVariantLookup{err: errors.New("inventory down")}, nil, zap.NewNop()))
	lines := []OrderLineInput{{SKU: "TSHIRT-RED-M", Metadata: map[string]any{LineVariantKey: "forged"}}}
	svc.StampLineVariants(context.Background(), uuid.New(), lines)
	if _, ok := lines[0].Metadata[LineVariantKey]; ok {
		t.Fatal("on lookup failure the line is left unstamped, never with the client's value")
	}
}

func TestStampLineVariantsWithoutResolver(t *testing.T) {
	svc := &Service{log: zap.NewNop()}
	lines := []OrderLineInput{{SKU: "X", Metadata: map[string]any{LineVariantKey: "forged"}}}
	svc.StampLineVariants(context.Background(), uuid.New(), lines)
	if _, ok := lines[0].Metadata[LineVariantKey]; ok {
		t.Fatal("client values are dropped even with no resolver wired")
	}
}
