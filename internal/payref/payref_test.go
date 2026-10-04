package payref

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestBuild_POSFormatDeterministic(t *testing.T) {
	tenant := uuid.MustParse("5bce71cd-a29f-484f-adc7-3566aed6d14f")
	order := uuid.MustParse("b2b59251-8e5d-4993-820e-7b140981d289")

	got := Build("POS", "Urban Loft Cafe", tenant, order)
	if got != "POS-URBANL-B2B592518E5D" {
		t.Fatalf("unexpected reference: %s", got)
	}
	// Deterministic per (svc,tenant,entity) so retries dedup in treasury.
	if Build("POS", "Urban Loft Cafe", tenant, order) != got {
		t.Fatal("not deterministic")
	}
}

func TestPortion_DistinctPerPaymentAndDeterministic(t *testing.T) {
	base := "POS-URBANL-B2B592518E5D"
	pay := uuid.MustParse("1a2b3c4d-0000-4000-8000-000000000000")
	if got := Portion(base, pay); got != "POS-URBANL-B2B592518E5D-P1A2B3C4D" {
		t.Fatalf("unexpected portion reference: %s", got)
	}
	if Portion(base, pay) != Portion(base, pay) {
		t.Fatal("not deterministic")
	}
	if Portion(base, uuid.New()) == Portion(base, uuid.New()) || Portion(base, pay) == base {
		t.Fatal("split portions must not share a reference")
	}
}

func TestBuild_SlugFallbackAndUniqueEntity(t *testing.T) {
	tenant := uuid.New()
	// Empty slug → tenant UUID hex fallback.
	if got := Build("POS", "", tenant, uuid.New()); !strings.HasPrefix(got, "POS-") || len(strings.Split(got, "-")) != 3 {
		t.Fatalf("bad fallback ref: %s", got)
	}
	// Distinct entities (e.g. per-folio-charge fresh UUIDs) → distinct refs.
	if Build("POS", "x", tenant, uuid.New()) == Build("POS", "x", tenant, uuid.New()) {
		t.Fatal("distinct entities collided")
	}
}
