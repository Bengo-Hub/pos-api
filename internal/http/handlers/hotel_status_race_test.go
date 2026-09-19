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
