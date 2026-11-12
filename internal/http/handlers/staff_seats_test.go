package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// Staff seats count ACTIVE staff only: a deactivated cashier frees its seat, so a tenant on a
// two-cashier plan can bring one back once another is deactivated, and reactivating while both
// seats are taken is refused like a new hire (2026-11-12).
func TestStaffSeatsCountActiveOnly(t *testing.T) {
	t.Setenv("AUTH_API_URL", "")
	h, client := newStaffTestHandler(t)
	tid := uuid.New()
	seedTenant(t, client, tid)
	outlet := seedOutlet(t, client, tid, "A")
	ctx := context.Background()
	admin := secClaims(tid, uuid.New(), "https://sso.example", false, "admin")
	admin.SubscriptionLimits = map[string]int{"max_staff": 10, "max_cashiers": 2}

	a := seedStaffMember(t, client, tid, "cashier", outlet.ID)
	b := seedStaffMember(t, client, tid, "cashier", outlet.ID)
	gone := seedStaffMember(t, client, tid, "cashier", outlet.ID)
	client.StaffMember.UpdateOne(gone).SetIsActive(false).ExecX(ctx)

	setActive := func(id uuid.UUID, active bool) int {
		rec := httptest.NewRecorder()
		h.UpdateStaff(rec, secRequest(t, http.MethodPatch, tid, map[string]any{"is_active": active}, admin,
			map[string]string{"staffID": id.String()}))
		return rec.Code
	}

	if code := setActive(gone.ID, true); code != http.StatusPaymentRequired {
		t.Fatalf("reactivating a third cashier with 2 active on a 2-seat plan: status = %d; want 402", code)
	}
	if client.StaffMember.GetX(ctx, gone.ID).IsActive {
		t.Fatal("a refused reactivation must leave the row inactive")
	}
	if code := setActive(b.ID, false); code != http.StatusNoContent {
		t.Fatalf("deactivating a cashier: status = %d", code)
	}
	if code := setActive(gone.ID, true); code != http.StatusNoContent {
		t.Fatalf("reactivating into a freed seat: status = %d; want 204", code)
	}
	if !client.StaffMember.GetX(ctx, a.ID).IsActive || !client.StaffMember.GetX(ctx, gone.ID).IsActive {
		t.Fatal("both seated cashiers must be active")
	}
}
