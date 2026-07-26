package shorttoken

import (
	"testing"

	"github.com/google/uuid"
)

func TestEncodeDecode_RoundTrip(t *testing.T) {
	for i := 0; i < 1000; i++ {
		id := uuid.New()
		code := Encode(id)
		got, err := Decode(code)
		if err != nil {
			t.Fatalf("decode(%q): %v", code, err)
		}
		if got != id {
			t.Fatalf("round-trip mismatch: %s -> %s -> %s", id, code, got)
		}
	}
}

func TestEncode_NilUUID(t *testing.T) {
	code := Encode(uuid.UUID{})
	got, err := Decode(code)
	if err != nil {
		t.Fatalf("decode nil-uuid code: %v", err)
	}
	if got != (uuid.UUID{}) {
		t.Fatalf("expected nil uuid, got %s", got)
	}
}

func TestDecode_Invalid(t *testing.T) {
	for _, bad := range []string{"", "0", "O", "I", "l", "not-base58-!"} {
		if _, err := Decode(bad); err == nil {
			t.Fatalf("expected error decoding %q", bad)
		}
	}
}
