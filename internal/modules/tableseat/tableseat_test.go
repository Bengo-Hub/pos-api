package tableseat

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestParseGuardMode(t *testing.T) {
	cases := map[any]GuardMode{
		"block": GuardBlock, " OFF ": GuardOff, "warn": GuardWarn, "": GuardWarn, nil: GuardWarn, 3: GuardWarn,
	}
	for in, want := range cases {
		if got := ParseGuardMode(in); got != want {
			t.Errorf("ParseGuardMode(%v) = %s, want %s", in, got, want)
		}
	}
}

func TestConflictErrorIs(t *testing.T) {
	var err error = &ConflictError{OrderID: uuid.New(), OrderNumber: "003390", Mode: GuardWarn}
	if !errors.Is(err, ErrTableHasOpenOrder) {
		t.Fatal("ConflictError must match ErrTableHasOpenOrder")
	}
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.OrderNumber != "003390" {
		t.Fatal("errors.As must recover the conflict details")
	}
}
