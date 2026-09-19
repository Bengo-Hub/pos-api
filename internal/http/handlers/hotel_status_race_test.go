package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	enthousekeeping "github.com/bengobox/pos-service/internal/ent/housekeepingtask"
	entroom "github.com/bengobox/pos-service/internal/ent/room"
	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
)

// withTestTenant carries the tenant id via httpware context the way the router's tenant
// middleware would, mirroring reportsRequest's (hotel_reports_test.go) equivalent for reports.
func withTestTenant(ctx context.Context, tid uuid.UUID) context.Context {
	return httpware.WithTenantID(ctx, tid.String())
}

// newHotelTestHandler mirrors newReportsTestHandler's sqlite-memory harness.
func newHotelTestHandler(t *testing.T) (*HotelHandler, *ent.Client) {
	t.Helper()
	client := openSQLiteTestClient(t, "hoteltest")
	return NewHotelHandler(zap.NewNop(), client, nil), client
}

// hotelRoomRequest builds a request carrying tenant context (httpware) and the room id as a chi
// URL param "id", the same shape the router provides at POST /{tenantID}/hotel/rooms/{id}/....
func hotelRoomRequest(t *testing.T, method string, tid, roomID uuid.UUID, body any) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode request body: %v", err)
		}
	}
	req := httptest.NewRequest(method, "/", &buf)
	req = req.WithContext(withTestTenant(req.Context(), tid))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", roomID.String())
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// TestCheckIn_ConcurrentRequests_OnlyOneSucceeds guards the double-booking race: CheckIn used to
// read the room's status, validate it, and only much later (after creating a guest + posting a
// folio charge) flip the room to occupied with an unconditional UpdateOne(room) -- two concurrent
// check-ins for the same room could both pass the early status check before either committed, and
// both would occupy the room with an "active" guest. The fix makes the final status flip a
// conditional bulk update (WHERE status IN (available, reserved)); this test fires N concurrent
// check-ins at the same available room and asserts exactly one succeeds.
func TestCheckIn_ConcurrentRequests_OnlyOneSucceeds(t *testing.T) {
	h, client := newHotelTestHandler(t)
	tid, outletID := uuid.New(), uuid.New()

	room, err := client.Room.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetRoomNumber("G1").
		SetName("G1").
		SetRatePerNight(800).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed room: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	var successes, conflicts int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := map[string]any{
				"guest_name": fmt.Sprintf("Guest %d", i),
				"phone":      "0700000000",
				"id_number":  fmt.Sprintf("ID-%d", i),
				"nights":     1,
			}
			req := hotelRoomRequest(t, http.MethodPost, tid, room.ID, body)
			rec := httptest.NewRecorder()
			h.CheckIn(rec, req)
			switch rec.Code {
			case http.StatusOK, http.StatusCreated:
				atomic.AddInt32(&successes, 1)
			case http.StatusConflict:
				atomic.AddInt32(&conflicts, 1)
			default:
				t.Errorf("unexpected status %d: %s", rec.Code, rec.Body.String())
			}
		}(i)
	}
	wg.Wait()

	if successes != 1 {
		t.Fatalf("expected exactly 1 successful check-in out of %d concurrent requests, got %d (conflicts=%d) -- double-booking race not prevented", n, successes, conflicts)
	}
	if conflicts != n-1 {
		t.Fatalf("expected %d conflicts, got %d", n-1, conflicts)
	}

	activeGuests, err := client.RoomGuest.Query().
		Where(entroomguest.TenantID(tid), entroomguest.RoomID(room.ID), entroomguest.StatusEQ(entroomguest.StatusActive)).
		Count(context.Background())
	if err != nil {
		t.Fatalf("count active guests: %v", err)
	}
	if activeGuests != 1 {
		t.Fatalf("expected exactly 1 active guest occupying the room after the race, got %d", activeGuests)
	}
}

// TestCheckOut_ConcurrentRequests_OnlyOneSucceeds guards the same race class on the way out:
// two concurrent checkout requests for the same guest (a double-click, or CheckOut racing
// SettleFolio's auto-checkout) used to both pass the "find active guest" lookup and both fire the
// checkout side effects. The fix conditions the guest-status flip on the guest still being active.
func TestCheckOut_ConcurrentRequests_OnlyOneSucceeds(t *testing.T) {
	h, client := newHotelTestHandler(t)
	tid, outletID := uuid.New(), uuid.New()

	room, err := client.Room.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetRoomNumber("G2").
		SetName("G2").
		SetRatePerNight(800).
		SetStatus(entroom.StatusOccupied).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed room: %v", err)
	}
	guest, err := client.RoomGuest.Create().
		SetTenantID(tid).
		SetRoomID(room.ID).
		SetGuestName("Test Guest").
		SetPhone("0700000000").
		SetIDNumber("ID-0001").
		SetCheckInDate(time.Now()).
		SetNights(1).
		SetCheckOutDate(time.Now().AddDate(0, 0, 1)).
		SetTotalRoomCharge(800).
		SetCheckedInBy(uuid.New()).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed room guest: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	var successes, conflicts int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := hotelRoomRequest(t, http.MethodPost, tid, room.ID, map[string]any{"checked_out_by": uuid.New().String()})
			rec := httptest.NewRecorder()
			h.CheckOut(rec, req)
			switch rec.Code {
			case http.StatusOK:
				atomic.AddInt32(&successes, 1)
			case http.StatusConflict:
				// Lost the race at the final conditional status update (guest.Status flipped
				// to checked_out by another request between this one's initial lookup and its
				// own update attempt).
				atomic.AddInt32(&conflicts, 1)
			case http.StatusNotFound:
				// Lost the race even earlier: CheckOut's initial "find active guest" lookup
				// itself already sees no active guest, because a faster request's checkout had
				// already fully committed by the time this one's read ran. Equally valid proof
				// the race is closed -- which of the two failure modes a loser hits is a timing
				// accident of goroutine/connection scheduling, not something either request
				// controls.
				atomic.AddInt32(&conflicts, 1)
			default:
				t.Errorf("unexpected status %d: %s", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()

	if successes != 1 {
		t.Fatalf("expected exactly 1 successful checkout out of %d concurrent requests, got %d (conflicts=%d)", n, successes, conflicts)
	}

	reloaded, err := client.RoomGuest.Get(context.Background(), guest.ID)
	if err != nil {
		t.Fatalf("reload guest: %v", err)
	}
	if reloaded.Status != entroomguest.StatusCheckedOut {
		t.Fatalf("expected guest to be checked_out, got %s", reloaded.Status)
	}

	// The checkout_clean task is created by a deliberately detached goroutine (see CheckOut's
	// context.WithoutCancel comment) that outlives the handler's own return, so it may not have
	// written yet right after wg.Wait() -- poll briefly instead of asserting immediately.
	var tasks int
	deadline := time.Now().Add(2 * time.Second)
	for {
		tasks, err = client.HousekeepingTask.Query().
			Where(enthousekeeping.TenantID(tid), enthousekeeping.RoomID(room.ID)).
			Count(context.Background())
		if err != nil {
			t.Fatalf("count housekeeping tasks: %v", err)
		}
		if tasks >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if tasks != 1 {
		t.Fatalf("expected exactly 1 checkout_clean task created (not one per racing request), got %d", tasks)
	}
}

// TestHousekeepingComplete_RoomStaysOutOfService_WhileASiblingTaskIsOpen guards the multi-task
// gap: a room can have more than one open task at once (the auto-created checkout_clean plus a
// separately logged maintenance task, say). Completing just one must not free the room while its
// sibling is still pending/in_progress.
func TestHousekeepingComplete_RoomStaysOutOfService_WhileASiblingTaskIsOpen(t *testing.T) {
	h, client := newHotelTestHandler(t)
	tid, outletID := uuid.New(), uuid.New()

	room, err := client.Room.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetRoomNumber("G3").
		SetName("G3").
		SetRatePerNight(800).
		SetStatus(entroom.StatusCleaning).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed room: %v", err)
	}

	cleanTask, err := client.HousekeepingTask.Create().
		SetTenantID(tid).SetRoomID(room.ID).
		SetTaskType(enthousekeeping.TaskTypeCheckoutClean).
		SetPriority(enthousekeeping.PriorityUrgent).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed checkout_clean task: %v", err)
	}
	maintTask, err := client.HousekeepingTask.Create().
		SetTenantID(tid).SetRoomID(room.ID).
		SetTaskType(enthousekeeping.TaskTypeMaintenance).
		SetPriority(enthousekeeping.PriorityNormal).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed maintenance task: %v", err)
	}

	completeTask := func(id uuid.UUID) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/", bytes.NewBufferString(`{"status":"completed"}`))
		req = req.WithContext(withTestTenant(req.Context(), tid))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("taskID", id.String())
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.UpdateHousekeepingTask(rec, req)
		return rec
	}

	if rec := completeTask(cleanTask.ID); rec.Code != http.StatusOK {
		t.Fatalf("complete checkout_clean task: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	reloaded, err := client.Room.Get(context.Background(), room.ID)
	if err != nil {
		t.Fatalf("reload room: %v", err)
	}
	if reloaded.Status != entroom.StatusCleaning {
		t.Fatalf("expected room to STAY cleaning while the maintenance task is still open, got %s", reloaded.Status)
	}

	if rec := completeTask(maintTask.ID); rec.Code != http.StatusOK {
		t.Fatalf("complete maintenance task: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	reloaded, err = client.Room.Get(context.Background(), room.ID)
	if err != nil {
		t.Fatalf("reload room: %v", err)
	}
	if reloaded.Status != entroom.StatusAvailable {
		t.Fatalf("expected room to become available once every open task is completed, got %s", reloaded.Status)
	}
}

// TestCheckIn_MissingPhone_ReturnsClearBadRequest guards the live-reported "failed to check in
// guest" bug: RoomGuest.phone is NotEmpty at the schema level, but only id_number had an explicit
// pre-check -- an empty phone (nothing in the UI blocked or marked it required) fell through to
// ent's raw validator error and 500'd with no indication of which field was the problem. Now
// guest_name and phone get the same clear-400 treatment id_number already had.
func TestCheckIn_MissingPhone_ReturnsClearBadRequest(t *testing.T) {
	h, client := newHotelTestHandler(t)
	tid, outletID := uuid.New(), uuid.New()

	room, err := client.Room.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetRoomNumber("G4").
		SetName("G4").
		SetRatePerNight(800).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed room: %v", err)
	}

	req := hotelRoomRequest(t, http.MethodPost, tid, room.ID, map[string]any{
		"guest_name": "Test Guest",
		"phone":      "",
		"id_number":  "ID-0001",
		"nights":     1,
	})
	rec := httptest.NewRecorder()
	h.CheckIn(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing phone, got %d: %s", rec.Code, rec.Body.String())
	}
	reloaded, err := client.Room.Get(context.Background(), room.ID)
	if err != nil {
		t.Fatalf("reload room: %v", err)
	}
	if reloaded.Status != entroom.StatusAvailable {
		t.Fatalf("expected room to remain available after a rejected check-in, got %s", reloaded.Status)
	}
	guestCount, err := client.RoomGuest.Query().Where(entroomguest.RoomID(room.ID)).Count(context.Background())
	if err != nil {
		t.Fatalf("count guests: %v", err)
	}
	if guestCount != 0 {
		t.Fatalf("expected no guest row created for a rejected check-in, got %d", guestCount)
	}
}

// TestOccupancySurchargePerNight_MatchesStandardHotelPMSRule is a pure unit test of the pricing
// arithmetic itself (base occupancy + extra-adult/child rates, standard hotel PMS practice — a
// room rate covers a base number of adults for free; each adult beyond that adds a per-night fee;
// a child below the free-age threshold is always free, one at or above it adds the child rate).
func TestOccupancySurchargePerNight_MatchesStandardHotelPMSRule(t *testing.T) {
	configured := bookingPolicy{BaseOccupancyAdults: 2, ExtraAdultRate: 500, ChildFreeUnderAge: 6, ExtraChildRate: 250}

	cases := []struct {
		name      string
		policy    bookingPolicy
		adults    int
		childAges []int
		want      float64
	}{
		{"disabled by default (BaseOccupancyAdults<=0) never charges extra", bookingPolicy{}, 6, []int{2, 9, 15}, 0},
		{"within base occupancy, no extra adults, no surcharge", configured, 2, nil, 0},
		{"one extra adult beyond base occupancy", configured, 3, nil, 500},
		{"child below free age is free even alone", configured, 2, []int{5}, 0},
		{"child at/above free age is chargeable", configured, 2, []int{8}, 250},
		{"mixed: one extra adult + one free child + one chargeable child", configured, 3, []int{5, 8}, 500 + 250},
		{"extra_child_rate=0 never charges children regardless of age", bookingPolicy{BaseOccupancyAdults: 2, ChildFreeUnderAge: 6, ExtraChildRate: 0}, 2, []int{10}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := occupancySurchargePerNight(c.policy, c.adults, c.childAges)
			if got != c.want {
				t.Fatalf("expected surcharge %.2f, got %.2f", c.want, got)
			}
		})
	}
}

// TestCheckIn_OccupancyPricing_UnconfiguredChargesFlatRateRegardlessOfHeadcount confirms the
// end-to-end wiring for the (default, common) disabled case: a property that has never configured
// occupancy pricing charges exactly the flat rate no matter how many adults/children check in --
// unchanged from before this feature existed. (The "configured" end-to-end case is covered by the
// pure unit test above; exercising the full outlet-settings FK chain here would test ent/seeding
// more than the actual pricing logic.)
func TestCheckIn_OccupancyPricing_UnconfiguredChargesFlatRateRegardlessOfHeadcount(t *testing.T) {
	h, client := newHotelTestHandler(t)
	ctx := context.Background()

	t.Run("unconfigured property charges the flat rate regardless of headcount", func(t *testing.T) {
		tid, outletID := uuid.New(), uuid.New()
		room, err := client.Room.Create().
			SetTenantID(tid).SetOutletID(outletID).SetRoomNumber("G6").SetName("G6").SetRatePerNight(800).
			Save(ctx)
		if err != nil {
			t.Fatalf("seed room: %v", err)
		}
		// No OutletSetting row at all -- resolveBookingPolicy falls back to defaultBookingPolicy(),
		// where BaseOccupancyAdults is 0 (disabled).

		req := hotelRoomRequest(t, http.MethodPost, tid, room.ID, map[string]any{
			"guest_name": "Legacy Test", "phone": "0700000001", "id_number": "ID-OCC-2",
			"nights": 2, "adults": 5, "children": 3, "child_ages": []int{2, 9, 15},
		})
		rec := httptest.NewRecorder()
		h.CheckIn(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("check-in: status = %d, body = %s", rec.Code, rec.Body.String())
		}

		guest, err := client.RoomGuest.Query().Where(entroomguest.RoomID(room.ID), entroomguest.StatusEQ(entroomguest.StatusActive)).Only(ctx)
		if err != nil {
			t.Fatalf("load guest: %v", err)
		}
		want := 1600.0 // 800 * 2 nights, unaffected by 5 adults + 3 children
		if guest.TotalRoomCharge != want {
			t.Fatalf("expected total_room_charge %.2f (flat rate, occupancy pricing off by default), got %.2f -- a property that never configured this must see NO change in behavior", want, guest.TotalRoomCharge)
		}
	})
}

// TestUpdateGuest_EditsDetailsAndExtendsStay covers the new "Edit Guest / Booking" endpoint:
// contact/ID corrections, occupancy, and extending a stay via either nights or an explicit
// departure date -- and confirms it does NOT touch total_room_charge (documented in UpdateGuest's
// own doc comment: this is a correction tool, not a re-billing one).
func TestUpdateGuest_EditsDetailsAndExtendsStay(t *testing.T) {
	h, client := newHotelTestHandler(t)
	tid, outletID := uuid.New(), uuid.New()
	ctx := context.Background()

	room, err := client.Room.Create().
		SetTenantID(tid).SetOutletID(outletID).SetRoomNumber("G7").SetName("G7").SetRatePerNight(800).
		SetStatus(entroom.StatusOccupied).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed room: %v", err)
	}
	checkIn := time.Date(2026, 1, 10, 14, 0, 0, 0, time.UTC)
	guest, err := client.RoomGuest.Create().
		SetTenantID(tid).SetRoomID(room.ID).
		SetGuestName("Original Name").SetPhone("0700000000").SetIDNumber("ID-EDIT-1").
		SetCheckInDate(checkIn).SetNights(2).SetCheckOutDate(checkIn.AddDate(0, 0, 2)).
		SetTotalRoomCharge(1600).SetCheckedInBy(uuid.New()).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed room guest: %v", err)
	}

	newPhone := "0711111111"
	newAdults := 3
	newAges := []int{7}
	req := hotelRoomRequest(t, http.MethodPatch, tid, room.ID, map[string]any{
		"phone": newPhone, "adults": newAdults, "child_ages": newAges, "nights": 4,
	})
	rec := httptest.NewRecorder()
	h.UpdateGuest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update guest: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	reloaded, err := client.RoomGuest.Get(ctx, guest.ID)
	if err != nil {
		t.Fatalf("reload guest: %v", err)
	}
	if reloaded.Phone != newPhone {
		t.Fatalf("expected phone %s, got %s", newPhone, reloaded.Phone)
	}
	if reloaded.Adults != newAdults {
		t.Fatalf("expected adults %d, got %d", newAdults, reloaded.Adults)
	}
	if len(reloaded.ChildAges) != 1 || reloaded.ChildAges[0] != 7 {
		t.Fatalf("expected child_ages [7], got %v", reloaded.ChildAges)
	}
	if reloaded.Nights != 4 {
		t.Fatalf("expected nights extended to 4, got %d", reloaded.Nights)
	}
	wantCheckout := checkIn.AddDate(0, 0, 4)
	if !reloaded.CheckOutDate.Equal(wantCheckout) {
		t.Fatalf("expected check_out_date %v, got %v", wantCheckout, reloaded.CheckOutDate)
	}
	// The whole point of this handler being a correction tool, not a re-billing one: extending
	// nights and adding an adult/child must NOT silently change what's already been charged.
	if reloaded.TotalRoomCharge != 1600 {
		t.Fatalf("expected total_room_charge to remain untouched at 1600, got %.2f -- UpdateGuest must never auto-adjust billing", reloaded.TotalRoomCharge)
	}

	// Rejects blanking a required field.
	req2 := hotelRoomRequest(t, http.MethodPatch, tid, room.ID, map[string]any{"phone": ""})
	rec2 := httptest.NewRecorder()
	h.UpdateGuest(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for blanking phone, got %d: %s", rec2.Code, rec2.Body.String())
	}
}
