package outletpolicy

import "strings"

// VoidReason is one entry of an outlet's void reason list (outlet settings metadata
// "void_reasons"). RecordWastage decides what a void does to stock: a duplicate or a mistake
// moves nothing, while food that was prepared and then refused is consumed as wastage so the
// stock count and variance reports stay true.
type VoidReason struct {
	Code          string `json:"code"`
	Label         string `json:"label"`
	RecordWastage bool   `json:"record_wastage"`
}

// VoidReasonsMetadataKey is the outlet settings metadata key holding the list.
const VoidReasonsMetadataKey = "void_reasons"

// DefaultVoidReasons apply until an outlet admin saves a list of their own.
func DefaultVoidReasons() []VoidReason {
	return []VoidReason{
		{Code: "duplicate", Label: "Duplicate order", RecordWastage: false},
		{Code: "entered_in_error", Label: "Entered in error", RecordWastage: false},
		{Code: "customer_cancelled", Label: "Customer cancelled before preparation", RecordWastage: false},
		{Code: "prepared_refused", Label: "Prepared, then refused or returned", RecordWastage: true},
	}
}

// VoidReasonsFromMetadata reads the outlet list, falling back to the defaults when it is
// missing or holds no usable entry.
func VoidReasonsFromMetadata(meta map[string]any) []VoidReason {
	raw, _ := meta[VoidReasonsMetadataKey].([]any)
	out := make([]VoidReason, 0, len(raw))
	seen := map[string]bool{}
	for _, item := range raw {
		m, _ := item.(map[string]any)
		code := strings.TrimSpace(strings.ToLower(asString(m["code"])))
		label := strings.TrimSpace(asString(m["label"]))
		if code == "" || label == "" || seen[code] {
			continue
		}
		seen[code] = true
		wastage, _ := m["record_wastage"].(bool)
		out = append(out, VoidReason{Code: code, Label: label, RecordWastage: wastage})
	}
	if len(out) == 0 {
		return DefaultVoidReasons()
	}
	return out
}

// FindVoidReason looks a code up in the outlet list. ok is false for an unknown or empty code,
// in which case the void keeps its free-text reason and never touches stock.
func FindVoidReason(meta map[string]any, code string) (VoidReason, bool) {
	code = strings.TrimSpace(strings.ToLower(code))
	if code == "" {
		return VoidReason{}, false
	}
	for _, r := range VoidReasonsFromMetadata(meta) {
		if r.Code == code {
			return r, true
		}
	}
	return VoidReason{}, false
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
