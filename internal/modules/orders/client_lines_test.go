package orders

import (
	"testing"

	"github.com/bengobox/pos-service/internal/ent"
)

func TestDropKnownClientLines(t *testing.T) {
	existing := []*ent.POSOrderLine{
		{Metadata: map[string]any{ClientLineIDKey: "a"}},
		{Metadata: nil},
	}
	incoming := []OrderLineInput{
		{SKU: "A", Metadata: map[string]any{ClientLineIDKey: "a"}}, // already on the bill
		{SKU: "B", Metadata: map[string]any{ClientLineIDKey: "b"}},
		{SKU: "B2", Metadata: map[string]any{ClientLineIDKey: "b"}}, // same id twice in one request
		{SKU: "C"}, // no id: always added
	}
	got := dropKnownClientLines(existing, incoming)
	if len(got) != 2 || got[0].SKU != "B" || got[1].SKU != "C" {
		t.Fatalf("unexpected lines: %+v", got)
	}
	if again := dropKnownClientLines(append(existing, &ent.POSOrderLine{Metadata: map[string]any{ClientLineIDKey: "b"}}), got[:1]); len(again) != 0 {
		t.Fatalf("replay must add nothing, got %+v", again)
	}
}
