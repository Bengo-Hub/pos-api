package orders

import (
	"strings"

	"github.com/bengobox/pos-service/internal/ent"
)

// ClientLineIDKey is the order line metadata key the till stamps on every line it adds to an
// existing bill, so add-to-bill is exactly-once across retries and offline replays.
const ClientLineIDKey = "client_line_id"

func clientLineID(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	s, _ := meta[ClientLineIDKey].(string)
	return strings.TrimSpace(s)
}

// dropKnownClientLines removes incoming lines whose client_line_id is already on the order, and
// duplicates of the same id within the request. Lines without an id are kept unchanged.
func dropKnownClientLines(existing []*ent.POSOrderLine, incoming []OrderLineInput) []OrderLineInput {
	seen := make(map[string]struct{}, len(existing))
	for _, l := range existing {
		if id := clientLineID(l.Metadata); id != "" {
			seen[id] = struct{}{}
		}
	}
	out := incoming[:0:0]
	for _, l := range incoming {
		id := clientLineID(l.Metadata)
		if id != "" {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
		}
		out = append(out, l)
	}
	return out
}
