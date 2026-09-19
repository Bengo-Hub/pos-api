package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
)

// editGuestInput is the editable subset of a RoomGuest's own details/occupancy plus the stay's
// dates — everything a front desk might reasonably need to correct after check-in (a mistyped
// phone number, a guest who forgot their email, an extra guest joining mid-stay) or extend/shorten
// (nights, departure date). Deliberately excludes fields that only make sense at check-in
// (check_in_date, source, booking_id, payment method) and total_room_charge, which this handler
// never touches directly — see UpdateGuest's doc comment for why.
type editGuestInput struct {
	GuestName           *string    `json:"guest_name"`
	FirstName           *string    `json:"first_name"`
	LastName            *string    `json:"last_name"`
	Email               *string    `json:"email"`
	Phone               *string    `json:"phone"`
	Nationality         *string    `json:"nationality"`
	IDType              *string    `json:"id_type"`
	IDNumber            *string    `json:"id_number"`
	IDDocumentURL       *string    `json:"id_document_url"`
	Adults              *int       `json:"adults"`
	Children            *int       `json:"children"`
	ChildAges           *[]int     `json:"child_ages"`
	Nights              *int       `json:"nights"`
	ExpectedDepartureAt *time.Time `json:"expected_departure_at"`
}

// UpdateGuest handles PATCH /{tenantID}/hotel/rooms/{id}/guest — edits the room's currently
// ACTIVE guest's own contact/ID details, occupancy, and stay dates (extend/shorten). This is a
// correction/amendment tool, not a re-billing one: it deliberately does NOT touch
// total_room_charge or post/adjust any RoomFolioItem, even when nights or occupancy changes,
// because doing that correctly needs to know which nights have already been folio-posted (varies
// by payment_timing — pay_upfront posts everything at check-in, per_day_split posts
// incrementally) to avoid double-charging or silently underbilling. A front desk that extends or
// shortens a stay, or adds a guest that changes the occupancy-pricing surcharge, posts the
// resulting adjustment via the existing "Add Folio Charge" action (POST .../folio) — the same
// tool already used for every other ad-hoc charge, so the adjustment shows up on the folio with
// its own clear description rather than silently mutating a number nobody sees change.
func (h *HotelHandler) UpdateGuest(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	roomID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid room id", http.StatusBadRequest)
		return
	}

	var input editGuestInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	guest, err := h.client.RoomGuest.Query().
		Where(entroomguest.TenantID(tid), entroomguest.RoomID(roomID), entroomguest.StatusEQ(entroomguest.StatusActive)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "no active guest for this room", http.StatusNotFound)
		return
	}

	update := guest.Update()
	if input.GuestName != nil {
		if strings.TrimSpace(*input.GuestName) == "" {
			jsonError(w, "guest_name cannot be blank", http.StatusBadRequest)
			return
		}
		update = update.SetGuestName(*input.GuestName)
	}
	if input.FirstName != nil {
		update = update.SetFirstName(*input.FirstName)
	}
	if input.LastName != nil {
		update = update.SetLastName(*input.LastName)
	}
	if input.Email != nil {
		update = update.SetEmail(*input.Email)
	}
	if input.Phone != nil {
		if strings.TrimSpace(*input.Phone) == "" {
			jsonError(w, "phone cannot be blank", http.StatusBadRequest)
			return
		}
		update = update.SetPhone(*input.Phone)
	}
	if input.Nationality != nil {
		update = update.SetNationality(*input.Nationality)
	}
	if input.IDType != nil {
		update = update.SetIDType(entroomguest.IDType(*input.IDType))
	}
	if input.IDNumber != nil {
		if strings.TrimSpace(*input.IDNumber) == "" {
			jsonError(w, "id_number cannot be blank", http.StatusBadRequest)
			return
		}
		update = update.SetIDNumber(*input.IDNumber)
	}
	if input.IDDocumentURL != nil {
		update = update.SetIDDocumentURL(*input.IDDocumentURL)
	}
	if input.Adults != nil {
		if *input.Adults < 1 {
			jsonError(w, "adults must be at least 1", http.StatusBadRequest)
			return
		}
		update = update.SetAdults(*input.Adults)
	}
	if input.Children != nil {
		if *input.Children < 0 {
			jsonError(w, "children cannot be negative", http.StatusBadRequest)
			return
		}
		update = update.SetChildren(*input.Children)
	}
	if input.ChildAges != nil {
		update = update.SetChildAges(*input.ChildAges)
	}

	// Nights/departure: either drives the other, same calendar-day convention the check-in form
	// and hotel_reports.go's occupancy calculations already use (see roomGuestNightInterval).
	switch {
	case input.Nights != nil:
		if *input.Nights < 1 {
			jsonError(w, "nights must be at least 1", http.StatusBadRequest)
			return
		}
		newDeparture := guest.CheckInDate.AddDate(0, 0, *input.Nights)
		update = update.SetNights(*input.Nights).SetCheckOutDate(newDeparture).SetExpectedDepartureAt(newDeparture)
	case input.ExpectedDepartureAt != nil:
		nights := calendarNightsBetween(guest.CheckInDate, *input.ExpectedDepartureAt)
		if nights < 1 {
			jsonError(w, "expected_departure_at must be after check-in", http.StatusBadRequest)
			return
		}
		update = update.SetNights(nights).SetCheckOutDate(*input.ExpectedDepartureAt).SetExpectedDepartureAt(*input.ExpectedDepartureAt)
	}

	updated, err := update.Save(r.Context())
	if err != nil {
		h.log.Error("update guest failed", zap.Error(err))
		jsonError(w, "failed to update guest", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// calendarNightsBetween mirrors the frontend's calendarDaysBetween and hotel_reports.go's
// roomGuestNightInterval convention: whole calendar-day nights, never a fractional wall-clock
// duration (see this session's nights-miscalculation fix for why that matters).
func calendarNightsBetween(checkIn, departure time.Time) int {
	ciDate := time.Date(checkIn.Year(), checkIn.Month(), checkIn.Day(), 0, 0, 0, 0, checkIn.Location())
	depDate := time.Date(departure.Year(), departure.Month(), departure.Day(), 0, 0, 0, 0, departure.Location())
	return int(depDate.Sub(ciDate).Hours() / 24)
}
