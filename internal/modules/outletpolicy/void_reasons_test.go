package outletpolicy

import "testing"

func TestVoidReasons(t *testing.T) {
	if got := VoidReasonsFromMetadata(nil); len(got) != len(DefaultVoidReasons()) {
		t.Fatalf("nil metadata must give defaults, got %d", len(got))
	}
	meta := map[string]any{VoidReasonsMetadataKey: []any{
		map[string]any{"code": "Dup", "label": "Duplicate", "record_wastage": false},
		map[string]any{"code": "dup", "label": "Again"},                            // duplicate code dropped
		map[string]any{"code": "spill", "label": "Spilled", "record_wastage": true}, //
		map[string]any{"code": "", "label": "No code"},                             // invalid dropped
	}}
	got := VoidReasonsFromMetadata(meta)
	if len(got) != 2 || got[0].Code != "dup" || !got[1].RecordWastage {
		t.Fatalf("unexpected reasons: %+v", got)
	}
	if r, ok := FindVoidReason(meta, "SPILL"); !ok || !r.RecordWastage {
		t.Fatal("lookup must be case-insensitive")
	}
	if _, ok := FindVoidReason(meta, "duplicate"); ok {
		t.Fatal("a custom list replaces the defaults")
	}
	if r, ok := FindVoidReason(nil, "prepared_refused"); !ok || !r.RecordWastage {
		t.Fatal("default prepared_refused records wastage")
	}
}
