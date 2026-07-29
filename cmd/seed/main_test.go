package main

import (
	"testing"

	"github.com/google/uuid"
)

// TestResolvePermissions_ExclusionPass verifies the new "!code" exclusion syntax added for the
// manager role's carve-out of pos.orders.delete/edit_finalized: a wildcard still grants
// everything else under the module, but an explicit "!exact.code" always wins, regardless of
// where it appears in the pattern list.
func TestResolvePermissions_ExclusionPass(t *testing.T) {
	permByCode := map[string]uuid.UUID{
		"pos.orders.add":            uuid.New(),
		"pos.orders.view":           uuid.New(),
		"pos.orders.delete":         uuid.New(),
		"pos.orders.edit_finalized": uuid.New(),
		"pos.orders.void_self":      uuid.New(),
		"pos.payments.add":          uuid.New(),
	}

	ids := resolvePermissions([]string{"pos.orders.*", "!pos.orders.delete", "!pos.orders.edit_finalized"}, permByCode)
	got := map[uuid.UUID]bool{}
	for _, id := range ids {
		got[id] = true
	}

	mustHave := []string{"pos.orders.add", "pos.orders.view", "pos.orders.void_self"}
	for _, code := range mustHave {
		if !got[permByCode[code]] {
			t.Errorf("expected %q to be included via the pos.orders.* wildcard, but it was not", code)
		}
	}
	mustNotHave := []string{"pos.orders.delete", "pos.orders.edit_finalized"}
	for _, code := range mustNotHave {
		if got[permByCode[code]] {
			t.Errorf("expected %q to be excluded by its \"!\" pattern, but it was included", code)
		}
	}
	if got[permByCode["pos.payments.add"]] {
		t.Errorf("expected pos.payments.add to be absent (no pattern matches it)")
	}
}

// TestResolvePermissions_WildcardStar verifies "*" still grants absolutely everything (admin's
// pattern) — the new exclusion pass must never touch a bare "*" role.
func TestResolvePermissions_WildcardStar(t *testing.T) {
	permByCode := map[string]uuid.UUID{
		"pos.orders.delete":         uuid.New(),
		"pos.orders.edit_finalized": uuid.New(),
	}
	ids := resolvePermissions([]string{"*"}, permByCode)
	if len(ids) != len(permByCode) {
		t.Fatalf("expected \"*\" to grant all %d permissions, got %d", len(permByCode), len(ids))
	}
}

// TestResolvePermissions_ExclusionOrderIndependent confirms an exclusion still applies even when
// listed BEFORE the wildcard that would otherwise grant it.
func TestResolvePermissions_ExclusionOrderIndependent(t *testing.T) {
	permByCode := map[string]uuid.UUID{
		"pos.orders.delete": uuid.New(),
		"pos.orders.add":    uuid.New(),
	}
	ids := resolvePermissions([]string{"!pos.orders.delete", "pos.orders.*"}, permByCode)
	for _, id := range ids {
		if id == permByCode["pos.orders.delete"] {
			t.Fatalf("expected pos.orders.delete to remain excluded regardless of pattern order")
		}
	}
}
