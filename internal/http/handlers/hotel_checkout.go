package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entroom "github.com/bengobox/pos-service/internal/ent/room"
	entroomfoliopayment "github.com/bengobox/pos-service/internal/ent/roomfoliopayment"
	entroomfolioitem "github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
	treasury "github.com/bengobox/pos-service/internal/modules/treasury"
)

// folioPaymentDTO is a payment row in the folio summary / history.
type folioPaymentDTO struct {
	ID        string    `json:"id"`
	Amount    float64   `json:"amount"`
	Currency  string    `json:"currency"`
	Method    string    `json:"method"`
	Reference string    `json:"reference,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// folioSummaryResponse is the full bill for a room's active guest: who booked, nights, the room
// charge, every folio charge, payments taken, and the outstanding balance — everything the checkout
// screen needs to show "the person who booked, the nights and the price" and settle the bill.
type folioSummaryResponse struct {
	RoomID        string            `json:"room_id"`
	RoomNumber    string            `json:"room_number"`
	RatePerNight  float64           `json:"rate_per_night"`
	GuestID       string            `json:"guest_id"`
	GuestName     string            `json:"guest_name"`
	Phone         string            `json:"phone,omitempty"`
	Nights        int               `json:"nights"`
	CheckInDate   time.Time         `json:"check_in_date"`
	CheckOutDate  time.Time         `json:"check_out_date"`
	RoomCharge    float64           `json:"room_charge"`
	ChargesTotal  float64           `json:"charges_total"`
	PaidTotal     float64           `json:"paid_total"`
	Balance       float64           `json:"balance"`
	Currency      string            `json:"currency"`
	Items         []folioItemDTO    `json:"items"`
	Payments      []folioPaymentDTO `json:"payments"`
}

type folioItemDTO struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Amount      float64   `json:"amount"`
	ChargeType  string    `json:"charge_type"`
	CreatedAt   time.Time `json:"created_at"`
}

// loadFolioSummary builds the bill for a room's active guest. Returns (nil, nil) when no active guest.
func (h *HotelHandler) loadFolioSummary(r *http.Request, tid, roomID uuid.UUID) (*folioSummaryResponse, error) {
	guest, err := h.client.RoomGuest.Query().
		Where(entroomguest.TenantID(tid), entroomguest.RoomID(roomID), entroomguest.StatusEQ(entroomguest.StatusActive)).
		Only(r.Context())
	if err != nil {
		return nil, nil // no active guest
	}

	items, err := h.client.RoomFolioItem.Query().
		Where(entroomfolioitem.TenantID(tid), entroomfolioitem.RoomGuestID(guest.ID)).
		Order(ent.Desc(entroomfolioitem.FieldCreatedAt)).
		All(r.Context())
	if err != nil {
		return nil, err
	}
	payments, err := h.client.RoomFolioPayment.Query().
		Where(entroomfoliopayment.TenantID(tid), entroomfoliopayment.RoomGuestID(guest.ID)).
		Order(ent.Desc(entroomfoliopayment.FieldCreatedAt)).
		All(r.Context())
	if err != nil {
		return nil, err
	}

	var chargesTotal, roomCharge float64
	itemDTOs := make([]folioItemDTO, 0, len(items))
	for _, it := range items {
		chargesTotal += it.Amount
		if it.ChargeType == entroomfolioitem.ChargeTypeRoomCharge {
			roomCharge += it.Amount
		}
		itemDTOs = append(itemDTOs, folioItemDTO{
			ID: it.ID.String(), Description: it.Description, Amount: it.Amount,
			ChargeType: string(it.ChargeType), CreatedAt: it.CreatedAt,
		})
	}
	var paidTotal float64
	payDTOs := make([]folioPaymentDTO, 0, len(payments))
	for _, p := range payments {
		if p.Status == "completed" {
			paidTotal += p.Amount
		}
		payDTOs = append(payDTOs, folioPaymentDTO{
			ID: p.ID.String(), Amount: p.Amount, Currency: p.Currency, Method: p.Method,
			Reference: p.Reference, Status: p.Status, CreatedAt: p.CreatedAt,
		})
	}

	currency := "KES"
	rate := 0.0
	roomNumber := ""
	if room, rerr := h.client.Room.Get(r.Context(), roomID); rerr == nil {
		rate = room.RatePerNight
		roomNumber = room.RoomNumber
		if room.Currency != "" {
			currency = room.Currency
		}
	}
	// Fall back to the guest's recorded room charge if no room_charge folio line exists yet.
	if roomCharge == 0 {
		roomCharge = guest.TotalRoomCharge
	}

	return &folioSummaryResponse{
		RoomID: roomID.String(), RoomNumber: roomNumber, RatePerNight: rate,
		GuestID: guest.ID.String(), GuestName: guest.GuestName, Phone: guest.Phone,
		Nights: guest.Nights, CheckInDate: guest.CheckInDate, CheckOutDate: guest.CheckOutDate,
		RoomCharge: roomCharge, ChargesTotal: chargesTotal, PaidTotal: paidTotal,
		Balance: chargesTotal - paidTotal, Currency: currency,
		Items: itemDTOs, Payments: payDTOs,
	}, nil
}

// GetFolioSummary handles GET /{tenantID}/hotel/rooms/{id}/folio/summary
func (h *HotelHandler) GetFolioSummary(w http.ResponseWriter, r *http.Request) {
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
	summary, err := h.loadFolioSummary(r, tid, roomID)
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if summary == nil {
		jsonError(w, "no active guest for this room", http.StatusNotFound)
		return
	}
	jsonOK(w, summary)
}

type settleFolioInput struct {
	Amount    float64 `json:"amount"`
	Method    string  `json:"method"`    // cash | card_manual | pdq | mpesa | mpesa_stk | card | wallet
	Reference string  `json:"reference"` // M-Pesa code / card approval ref (optional)
	Checkout  bool    `json:"checkout"`  // when true, check the guest out if the balance clears
}

// immediate-settle hotel tenders: cashier collects at the desk (cash / external card terminal / a
// confirmed M-Pesa code), so the payment is recorded completed with no online gateway round-trip.
func isImmediateHotelMethod(method string) bool {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "cash", "card_manual", "pdq", "card_terminal", "mpesa", "manual":
		return true
	default:
		return false
	}
}

func treasuryMethodForHotel(method string) string {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "card_manual", "pdq", "card_terminal":
		return "card_manual"
	case "cash", "mpesa", "manual":
		return "cash" // immediate settle; M-Pesa code is reconciled like cash at the desk
	case "mpesa_stk":
		return "mpesa" // online STK push
	default:
		return strings.ToLower(strings.TrimSpace(method))
	}
}

// SettleFolio handles POST /{tenantID}/hotel/rooms/{id}/settle — records a payment against the
// active guest's folio (full or partial), capturing it in treasury and in the local payment history.
// When `checkout` is set and the balance clears, the guest is checked out and the room sent to cleaning.
func (h *HotelHandler) SettleFolio(w http.ResponseWriter, r *http.Request) {
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
	var input settleFolioInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Amount <= 0 {
		jsonError(w, "amount must be positive", http.StatusBadRequest)
		return
	}
	if input.Method == "" {
		jsonError(w, "method is required", http.StatusBadRequest)
		return
	}

	guest, err := h.client.RoomGuest.Query().
		Where(entroomguest.TenantID(tid), entroomguest.RoomID(roomID), entroomguest.StatusEQ(entroomguest.StatusActive)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "no active guest for this room", http.StatusNotFound)
		return
	}

	tenantSlug := chi.URLParam(r, "tenantID")
	immediate := isImmediateHotelMethod(input.Method)
	status := "pending"
	if immediate {
		status = "completed"
	}

	// Capture in treasury (immediate-settle for cash/card; pending intent for online gateways).
	var intentID, initiateURL string
	if h.treasuryClient != nil {
		intentReq := treasury.CreateIntentRequest{
			SourceService: "pos",
			ReferenceID:   fmt.Sprintf("%s:%d", guest.ID.String(), time.Now().UnixNano()),
			ReferenceType: "hotel_folio",
			Amount:        input.Amount,
			Currency:      "KES",
			PaymentMethod: immediateOrPending(immediate, treasuryMethodForHotel(input.Method)),
			Description:   fmt.Sprintf("Hotel folio payment - %s", guest.GuestName),
			OutletID:      "",
			Metadata: map[string]any{"room_id": roomID.String(), "guest_id": guest.ID.String(), "method": input.Method},
		}
		if input.Reference != "" {
			intentReq.Metadata["external_ref"] = input.Reference
		}
		intent, ierr := h.treasuryClient.CreateIntent(r.Context(), tenantSlug, intentReq.ReferenceID, intentReq)
		if ierr != nil {
			h.log.Warn("hotel folio: treasury intent failed", zap.Error(ierr))
			if !immediate {
				jsonError(w, "could not start payment", http.StatusBadGateway)
				return
			}
		} else {
			intentID = intent.ResolvedID()
			initiateURL = intent.InitiateURL
		}
	}

	recordedBy, _ := uuid.Parse(r.Header.Get("X-User-ID"))
	create := h.client.RoomFolioPayment.Create().
		SetTenantID(tid).
		SetRoomID(roomID).
		SetRoomGuestID(guest.ID).
		SetAmount(input.Amount).
		SetMethod(strings.ToLower(strings.TrimSpace(input.Method))).
		SetStatus(status)
	if input.Reference != "" {
		create = create.SetReference(input.Reference)
	}
	if intentID != "" {
		create = create.SetTreasuryIntentID(intentID)
	}
	if recordedBy != uuid.Nil {
		create = create.SetRecordedBy(recordedBy)
	}
	if _, err := create.Save(r.Context()); err != nil {
		h.log.Error("hotel folio: record payment failed", zap.Error(err))
		jsonError(w, "failed to record payment", http.StatusInternalServerError)
		return
	}

	summary, _ := h.loadFolioSummary(r, tid, roomID)

	// Auto-checkout when settled in full and requested.
	checkedOut := false
	if input.Checkout && immediate && summary != nil && summary.Balance <= 0.009 {
		now := time.Now()
		if _, uerr := h.client.RoomGuest.UpdateOne(guest).
			SetStatus(entroomguest.StatusCheckedOut).
			SetNillableCheckedOutBy(&recordedBy).
			SetCheckedOutAt(now).Save(r.Context()); uerr == nil {
			_, _ = h.client.Room.UpdateOneID(roomID).SetStatus(entroom.StatusCleaning).Save(r.Context())
			checkedOut = true
			if h.publisher != nil {
				_ = h.publisher.PublishHotelCheckOut(r.Context(), tid, map[string]any{
					"room_id": roomID, "guest_id": guest.ID, "guest_name": guest.GuestName,
					"total_folio": summary.ChargesTotal, "checked_out_at": now,
				})
			}
			go func() {
				gid := guest.ID
				_, _ = h.client.HousekeepingTask.Create().
					SetTenantID(tid).SetRoomID(roomID).SetNillableRoomGuestID(&gid).
					SetTaskType("checkout_clean").SetPriority("urgent").Save(r.Context())
			}()
		}
	}

	jsonOK(w, map[string]any{
		"status":       status,
		"checked_out":  checkedOut,
		"intent_id":    intentID,
		"initiate_url": initiateURL,
		"summary":      summary,
	})
}

func immediateOrPending(immediate bool, method string) string {
	if immediate {
		return method
	}
	return method // online gateway methods (mpesa/card/wallet) are initiated by treasury as usual
}
