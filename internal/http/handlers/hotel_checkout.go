package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Bengo-Hub/httpware/money"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entroom "github.com/bengobox/pos-service/internal/ent/room"
	entroomfolioitem "github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	entroomfoliopayment "github.com/bengobox/pos-service/internal/ent/roomfoliopayment"
	entroomguest "github.com/bengobox/pos-service/internal/ent/roomguest"
	"github.com/bengobox/pos-service/internal/modules/payments"
	treasury "github.com/bengobox/pos-service/internal/modules/treasury"
	"github.com/bengobox/pos-service/internal/payref"
)

// folioPaymentDTO is a payment row in the folio summary / history.
type folioPaymentDTO struct {
	ID        string    `json:"id"`
	Amount    float64   `json:"amount"`
	Currency  string    `json:"currency"`
	Method    string    `json:"method"`
	Reference string    `json:"reference,omitempty"`
	Label     string    `json:"label,omitempty"` // how the guest paid, for other_manual
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// folioSummaryResponse is the full bill for a room's active guest: who booked, nights, the room
// charge, every folio charge, payments taken, and the outstanding balance — everything the checkout
// screen needs to show "the person who booked, the nights and the price" and settle the bill.
type folioSummaryResponse struct {
	RoomID       string            `json:"room_id"`
	RoomNumber   string            `json:"room_number"`
	RatePerNight float64           `json:"rate_per_night"`
	GuestID      string            `json:"guest_id"`
	GuestName    string            `json:"guest_name"`
	Phone        string            `json:"phone,omitempty"`
	Nights       int               `json:"nights"`
	CheckInDate  time.Time         `json:"check_in_date"`
	CheckOutDate time.Time         `json:"check_out_date"`
	RoomCharge   float64           `json:"room_charge"`
	ChargesTotal float64           `json:"charges_total"`
	PaidTotal    float64           `json:"paid_total"`
	Balance      float64           `json:"balance"`
	Currency     string            `json:"currency"`
	Items        []folioItemDTO    `json:"items"`
	Payments     []folioPaymentDTO `json:"payments"`
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
	return h.folioSummaryFor(r.Context(), h.client, tid, roomID, guest)
}

// folioSummaryFor builds the bill for guest from db (the plain client, or a transaction's client
// so SettleFolio can read the payment it has not committed yet).
func (h *HotelHandler) folioSummaryFor(ctx context.Context, db *ent.Client, tid, roomID uuid.UUID, guest *ent.RoomGuest) (*folioSummaryResponse, error) {
	items, err := db.RoomFolioItem.Query().
		Where(entroomfolioitem.TenantID(tid), entroomfolioitem.RoomGuestID(guest.ID)).
		Order(ent.Desc(entroomfolioitem.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	folioPays, err := db.RoomFolioPayment.Query().
		Where(entroomfoliopayment.TenantID(tid), entroomfoliopayment.RoomGuestID(guest.ID)).
		Order(ent.Desc(entroomfoliopayment.FieldCreatedAt)).
		All(ctx)
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
	payDTOs := make([]folioPaymentDTO, 0, len(folioPays))
	for _, p := range folioPays {
		if p.Status == "completed" {
			paidTotal += p.Amount
		}
		label, _ := p.Metadata["payment_label"].(string)
		payDTOs = append(payDTOs, folioPaymentDTO{
			ID: p.ID.String(), Amount: p.Amount, Currency: p.Currency, Method: p.Method,
			Reference: p.Reference, Label: label, Status: p.Status, CreatedAt: p.CreatedAt,
		})
	}

	currency := "KES"
	rate := 0.0
	roomNumber := ""
	if room, rerr := db.Room.Get(ctx, roomID); rerr == nil {
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
	Amount float64 `json:"amount"`
	// Method: cash | card_manual | pdq | mpesa | mtn_momo_manual | airtel_money_manual |
	// other_manual | mpesa_stk | card | wallet
	Method string `json:"method"`
	// Reference is the transaction code (required for M-Pesa, MTN MoMo and Airtel Money, checked
	// like the till), a card approval ref, or an optional reference for Other.
	Reference string `json:"reference"`
	// Label says how the guest paid; required for other_manual (2 to 40 characters).
	Label    string `json:"label"`
	Checkout bool   `json:"checkout"` // when true, check the guest out if the balance clears
}

// immediate-settle hotel tenders: cashier collects at the desk (cash / external card terminal / a
// confirmed M-Pesa, MTN MoMo or Airtel Money code / Other), so the payment is recorded completed
// with no online gateway round-trip.
func isImmediateHotelMethod(method string) bool {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "cash", "card_manual", "pdq", "card_terminal", "mpesa", "manual", "mpesa_manual",
		payments.TenderMTNMoMoManual, payments.TenderAirtelMoneyManual, payments.TenderOtherManual:
		return true
	default:
		return false
	}
}

// deskTender is a hotel desk payment after the till's code checks.
type deskTender struct {
	Method    string // as the desk sent it, lowercased (stored on the folio payment)
	Reference string
	Label     string // only for other_manual
}

// deskCodeMethod maps a desk method onto the till tender whose code rules apply, or "" when the
// method carries no checked code. The desk has long sent a sighted M-Pesa code as "mpesa".
func deskCodeMethod(method string) string {
	switch method {
	case "mpesa", "manual", "mpesa_manual":
		return "mpesa_manual"
	case payments.TenderMTNMoMoManual, payments.TenderAirtelMoneyManual, payments.TenderOtherManual:
		return method
	}
	return ""
}

// checkDeskTender applies the till's rules to a hotel desk payment (owner decision 2026-11-12):
// an M-Pesa code must be present, MTN MoMo and Airtel Money IDs must have the transaction ID
// shape, Other needs a 2 to 40 character label, and an M-Pesa, MTN or Airtel code that already
// settled a till sale or a hotel folio in this tenant is refused. Codes are uppercased. Other
// methods pass through with the reference as sent. On refusal it returns the HTTP status to
// answer with (400 for the format, 409 for a reused code, 500 when the check could not run).
func (h *HotelHandler) checkDeskTender(ctx context.Context, tid uuid.UUID, method, reference, label string) (deskTender, int, error) {
	method = strings.ToLower(strings.TrimSpace(method))
	codeMethod := deskCodeMethod(method)
	if codeMethod == "" {
		return deskTender{Method: method, Reference: reference}, 0, nil
	}
	_, ref, lbl, err := payments.NormalizeManualCode(codeMethod, reference, label)
	if err != nil {
		return deskTender{}, http.StatusBadRequest, err
	}
	used, err := payments.ManualCodeUsed(ctx, h.client, tid, codeMethod, ref)
	if err != nil {
		h.log.Error("hotel folio: code reuse check failed", zap.String("tenant_id", tid.String()), zap.Error(err))
		return deskTender{}, http.StatusInternalServerError, errors.New("could not check the payment code, try again")
	}
	if used {
		return deskTender{}, http.StatusConflict, deskCodeUsedError(codeMethod, ref)
	}
	return deskTender{Method: method, Reference: ref, Label: lbl}, 0, nil
}

// errDeskCodeUsed marks a desk payment refused because its code already settled another sale
// or bill; callers answer 409 with the error's own message.
var errDeskCodeUsed = errors.New("payment code already used")

type deskCodeUsed struct{ msg string }

func (e deskCodeUsed) Error() string        { return e.msg }
func (e deskCodeUsed) Is(target error) bool { return target == errDeskCodeUsed }

func deskCodeUsedError(codeMethod, ref string) error {
	return deskCodeUsed{msg: fmt.Sprintf("%s code %s has already been used to settle another sale or bill",
		payments.ManualTenderName(codeMethod), ref)}
}

// recheckDeskCode runs the duplicate check again right before a desk payment is written, which
// closes most of the window between checkDeskTender and the write (the pay upfront check-in runs
// the CRM link and the check-in transaction in between). Like the till's check it is not a lock.
func (h *HotelHandler) recheckDeskCode(ctx context.Context, db *ent.Client, tid uuid.UUID, tender deskTender) error {
	codeMethod := deskCodeMethod(tender.Method)
	if codeMethod == "" || tender.Reference == "" {
		return nil
	}
	used, err := payments.ManualCodeUsed(ctx, db, tid, codeMethod, tender.Reference)
	if err != nil {
		return fmt.Errorf("check the payment code: %w", err)
	}
	if used {
		return deskCodeUsedError(codeMethod, tender.Reference)
	}
	return nil
}

func treasuryMethodForHotel(method string) string {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "card_manual", "pdq", "card_terminal":
		return "card_manual"
	case "cash":
		return "cash"
	case "mpesa", "manual", "mpesa_manual":
		// A sighted M-Pesa Paybill/Till code settles immediately but must stay identifiable as
		// M-Pesa money (never folded into "cash" — that made cash-vs-M-Pesa reconciliation
		// impossible; fixed 2026-07-13 alongside the POS terminal's identical bug).
		return "mpesa_manual"
	case "mpesa_stk":
		return "mpesa" // online STK push
	default:
		// mtn_momo_manual, airtel_money_manual and other_manual: treasury settles these codes
		// immediately as themselves, as at the till.
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
	tender, httpCode, terr := h.checkDeskTender(r.Context(), tid, input.Method, input.Reference, input.Label)
	if terr != nil {
		jsonError(w, terr.Error(), httpCode)
		return
	}

	guest, err := h.client.RoomGuest.Query().
		Where(entroomguest.TenantID(tid), entroomguest.RoomID(roomID), entroomguest.StatusEQ(entroomguest.StatusActive)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "no active guest for this room", http.StatusNotFound)
		return
	}

	// RoomGuest doesn't carry outlet_id itself (only Room does) — resolve it so hotel revenue
	// gets the SAME per-outlet GL attribution every normal POS sale already gets (see
	// payments/service.go's OutletID: order.OutletID.String() for the equivalent non-hotel path).
	// Without this, a mixed retail+hospitality tenant's guest-house revenue was silently
	// unattributed to any outlet in treasury's ledger and per-outlet P&L.
	room, err := h.client.Room.Get(r.Context(), roomID)
	if err != nil {
		jsonError(w, "room not found", http.StatusNotFound)
		return
	}

	tenantSlug := chi.URLParam(r, "tenantID")
	immediate := isImmediateHotelMethod(input.Method)
	status := "pending"
	if immediate {
		status = "completed"
	}

	// One transaction under the stay lock (review M-1): the split below reads the folio, and the
	// payment row it writes is the next payment's input, so two payments taken at the same moment
	// (a double submit, or two desks) are serialized and never both claim the POS share. A room
	// charge or checkout on the same stay waits for this to commit. Lock order: stay only, no
	// order row is taken here.
	ctx := r.Context()
	tx, err := h.client.Tx(ctx)
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	rollback := func() { _ = tx.Rollback() }
	guest, err = payments.LockActiveStay(ctx, tx, tid, guest.ID)
	if err != nil {
		rollback()
		if ent.IsNotFound(err) {
			jsonError(w, "no active guest for this room", http.StatusNotFound)
			return
		}
		h.log.Error("hotel folio: lock stay failed", zap.String("room_id", roomID.String()), zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// GL split of this payment (treasury's PostPaymentToLedgerItemized): the share that clears POS
	// sales already charged to the room (pos_charge_amount, booked to the guest receivable at the
	// till since S2-20, so it must not be revenue again), then the room-vs-ancillary split of the
	// rest. Pending payments are read too: their POS share is reserved (folioPaymentSplit).
	folioItems, err := tx.RoomFolioItem.Query().
		Where(entroomfolioitem.TenantID(tid), entroomfolioitem.RoomGuestID(guest.ID)).
		All(ctx)
	if err != nil {
		rollback()
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	folioPays, err := tx.RoomFolioPayment.Query().
		Where(entroomfoliopayment.TenantID(tid), entroomfoliopayment.RoomGuestID(guest.ID),
			entroomfoliopayment.StatusIn("completed", "pending")).
		All(ctx)
	if err != nil {
		rollback()
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	posChargeAmount, roomRevenueAmount := folioPaymentSplit(folioItems, folioPays, input.Amount)

	// Capture in treasury (immediate-settle for cash/card; pending intent for online gateways)
	// and in the local payment ledger — shared with CheckIn's pay_upfront path.
	recordedBy, _ := uuid.Parse(r.Header.Get("X-User-ID"))
	intentID, initiateURL, _, perr := h.recordFolioPayment(r, tx.Client(), tid, tenantSlug, room, guest.ID, guest.GuestName, input.Amount, tender, roomRevenueAmount, posChargeAmount, recordedBy)
	if perr != nil {
		rollback()
		if errors.Is(perr, errDeskCodeUsed) {
			jsonError(w, perr.Error(), http.StatusConflict)
			return
		}
		if !immediate {
			h.log.Warn("hotel folio: treasury intent failed", zap.Error(perr))
			jsonError(w, "could not start payment", http.StatusBadGateway)
			return
		}
		h.log.Error("hotel folio: record payment failed", zap.Error(perr))
		jsonError(w, "failed to record payment", http.StatusInternalServerError)
		return
	}

	// Auto-checkout when requested and the payment cleared the folio: under the same stay lock,
	// after the payment above, with the balance read again in this transaction (review R-5). A
	// folio that still owes anything stays checked in and the payment is kept.
	checkedOut := false
	now := time.Now()
	if input.Checkout && immediate {
		_, done, cerr := checkOutLocked(ctx, tx, guest, recordedBy, now)
		if cerr != nil {
			rollback()
			h.log.Error("hotel folio: settle and check out failed", zap.String("guest_id", guest.ID.String()), zap.Error(cerr))
			jsonError(w, "failed to record payment", http.StatusInternalServerError)
			return
		}
		checkedOut = done
	}
	// Read in the transaction: after a checkout the stay is no longer active, so the summary
	// lookup by room would find nothing once this commits.
	summary, serr := h.folioSummaryFor(ctx, tx.Client(), tid, roomID, guest)
	if serr != nil {
		h.log.Warn("hotel folio: summary after payment failed", zap.String("guest_id", guest.ID.String()), zap.Error(serr))
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("hotel folio: commit failed", zap.String("guest_id", guest.ID.String()), zap.Error(err))
		jsonError(w, "failed to record payment", http.StatusInternalServerError)
		return
	}

	if checkedOut {
		total := 0.0
		if summary != nil {
			total = summary.ChargesTotal
		}
		if h.publisher != nil {
			_ = h.publisher.PublishHotelCheckOut(r.Context(), tid, withGuestRecipient(map[string]any{
				"room_id": roomID, "guest_id": guest.ID,
				"total_folio": total, "checked_out_at": now,
			}, guest, guest.GuestName))
		}
		// context.WithoutCancel: see CheckOut's identical detached task-creation comment in
		// hotel.go — r.Context() would otherwise race the server's post-handler cancellation.
		bgCtx := context.WithoutCancel(r.Context())
		go func() {
			gid := guest.ID
			_, _ = h.client.HousekeepingTask.Create().
				SetTenantID(tid).SetRoomID(roomID).SetNillableRoomGuestID(&gid).
				SetTaskType("checkout_clean").SetPriority("urgent").Save(bgCtx)
		}()
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

// recordFolioPayment captures a payment against a room guest's folio: a treasury payment
// intent (when a treasury client is configured, outlet-attributed for correct per-outlet GL
// posting) plus a local RoomFolioPayment row. Shared by SettleFolio (checkout) and CheckIn's
// pay_upfront booking-policy path so every hotel settlement point gets identical treasury/GL
// behaviour instead of two copies drifting apart. db is where the folio payment is written: the
// plain client, or SettleFolio's transaction (which holds the stay lock).
func (h *HotelHandler) recordFolioPayment(
	r *http.Request, db *ent.Client, tid uuid.UUID, tenantSlug string, room *ent.Room, guestID uuid.UUID, guestName string,
	amount float64, tender deskTender, roomRevenueAmount, posChargeAmount float64, recordedBy uuid.UUID,
) (intentID, initiateURL, status string, err error) {
	method, reference := tender.Method, tender.Reference
	if err := h.recheckDeskCode(r.Context(), db, tid, tender); err != nil {
		return "", "", "", err
	}
	immediate := isImmediateHotelMethod(method)
	var pendingIntent map[string]any
	status = "pending"
	if immediate {
		status = "completed"
	}

	if h.treasuryClient != nil {
		intentReq := treasury.CreateIntentRequest{
			SourceService: "pos",
			ReferenceID:   payref.Build("POS", tenantSlug, uuid.Nil, uuid.New()),
			ReferenceType: "hotel_folio",
			Amount:        amount,
			Currency:      "KES",
			PaymentMethod: immediateOrPending(immediate, treasuryMethodForHotel(method)),
			Description:   fmt.Sprintf("Hotel folio payment - %s", guestName),
			OutletID:      room.OutletID.String(),
			Metadata: map[string]any{
				"service": "pos", "room_id": room.ID.String(), "guest_id": guestID.String(), "entity_id": guestID.String(), "method": method,
				"room_revenue_amount": fmt.Sprintf("%.2f", roomRevenueAmount),
			},
		}
		// The share clearing POS sales charged to the room: treasury credits the guest
		// receivable for it instead of revenue (S2-20).
		if posChargeAmount > 0.009 {
			intentReq.Metadata["pos_charge_amount"] = fmt.Sprintf("%.2f", posChargeAmount)
		}
		if reference != "" {
			intentReq.Metadata["external_ref"] = reference
		}
		if tender.Label != "" {
			intentReq.Metadata["payment_label"] = tender.Label
		}
		intent, ierr := h.treasuryClient.CreateIntent(r.Context(), tenantSlug, intentReq.ReferenceID, intentReq)
		if ierr != nil {
			h.log.Warn("hotel folio: treasury intent failed", zap.Error(ierr))
			if !immediate {
				return "", "", "", ierr
			}
			// The money is in hand, so the folio records it; the intent is kept and sent again by
			// payments.RetryPendingFolioIntents with the same reference id (S2-20 review M-10).
			pendingIntent = payments.PendingFolioIntent(intentReq)
		} else {
			intentID = intent.ResolvedID()
			initiateURL = intent.InitiateURL
		}
	}

	create := db.RoomFolioPayment.Create().
		SetTenantID(tid).
		SetRoomID(room.ID).
		SetRoomGuestID(guestID).
		SetAmount(amount).
		SetMethod(strings.ToLower(strings.TrimSpace(method))).
		SetStatus(status)
	if reference != "" {
		create = create.SetReference(reference)
	}
	md := map[string]any{}
	if tender.Label != "" {
		md["payment_label"] = tender.Label
	}
	// Kept on the folio payment so the next payment's split knows how much of the POS
	// receivable this one already cleared (folioPaymentSplit).
	if posChargeAmount > 0.009 {
		md[folioPOSChargeKey] = money.Round2(posChargeAmount)
	}
	if pendingIntent != nil {
		md[payments.MetaFolioIntentPending] = pendingIntent
	}
	if len(md) > 0 {
		create = create.SetMetadata(md)
	}
	if intentID != "" {
		create = create.SetTreasuryIntentID(intentID)
	}
	if recordedBy != uuid.Nil {
		create = create.SetRecordedBy(recordedBy)
	}
	if _, cerr := create.Save(r.Context()); cerr != nil {
		return intentID, initiateURL, status, cerr
	}
	return intentID, initiateURL, status, nil
}

// folioPOSChargeKey is the RoomFolioPayment metadata key holding the share of that payment that
// cleared POS sales charged to the room (the pos_charge_amount sent to treasury).
const folioPOSChargeKey = "pos_charge_amount"

// folioPaymentSplit works out, for a folio payment of amount, the share that clears POS sales
// charged to the room (posShare) and the room-revenue share of the rest (roomShare).
//
// Since S2-20 the till books a room-charged sale to the guest receivable (Dr receivable / Cr
// revenue), so at checkout treasury credits that receivable for posShare instead of booking the
// revenue a second time. What the folio still owes on POS sales is their lines (the ones the
// till's room charge wrote, payments.IsRoomChargeFolioLine) less the return credits already made against them (method return_credit, which
// treasury booked against the same receivable) less what earlier payments already cleared
// (their pos_charge_amount metadata). A payment is applied to the POS lines last: it clears POS
// value only once the rest of the folio is paid, so a payment of the whole balance clears exactly
// the POS receivable and a part payment clears none of it until it reaches the POS lines. The
// rest splits room vs other charges in proportion to the non-POS lines, the same mix the room
// split always used.
//
// A pending payment (an online payment still in flight) counts as if it will succeed (review
// M-1): its amount is taken off what the folio owes and its pos_charge_amount is reserved. A
// payment taken while another is pending therefore never claims the same POS share; if the
// pending one fails, treasury books nothing for it, the row turns failed, and the next payment
// claims what it had reserved. Failed payments are not counted. SettleFolio holds the stay lock
// while it computes this and writes the payment, so two payments never read the same state.
func folioPaymentSplit(items []*ent.RoomFolioItem, pays []*ent.RoomFolioPayment, amount float64) (posShare, roomShare float64) {
	if amount <= 0 {
		return 0, 0
	}
	var total, room, pos float64
	for _, it := range items {
		total += it.Amount
		switch {
		case payments.IsRoomChargeFolioLine(it):
			pos += it.Amount
		case it.ChargeType == entroomfolioitem.ChargeTypeRoomCharge:
			room += it.Amount
		}
	}
	var paid, posCleared float64
	for _, p := range pays {
		if p.Status != "completed" && p.Status != "pending" {
			continue
		}
		paid += p.Amount
		if p.Method == payments.FolioReturnCreditMethod {
			posCleared += p.Amount
			continue
		}
		if v, ok := p.Metadata[folioPOSChargeKey].(float64); ok && v > 0 {
			posCleared += v
		}
	}
	outstanding := total - paid
	posOwed := pos - posCleared
	if posOwed > outstanding {
		posOwed = outstanding
	}
	// The payment goes to the room nights and other lines first and to the POS lines last
	// (review I-2): only what is left once everything else is paid clears the POS receivable. A
	// part payment therefore never clears POS value that a later return credit also clears.
	if posOwed > 0.009 {
		posShare = amount - (outstanding - posOwed)
		if posShare > posOwed {
			posShare = posOwed
		}
		if posShare < 0 {
			posShare = 0
		}
		posShare = money.Round2(posShare)
	}
	if nonPOS := total - pos; nonPOS > 0.009 {
		roomShare = money.Round2((amount - posShare) * room / nonPOS)
	}
	return posShare, roomShare
}

// folioOwedMessage is the refusal shown when a guest is checked out with something still owed.
func folioOwedMessage(owed float64, currency string) string {
	return fmt.Sprintf("This guest still owes %s %.2f on the folio. Take payment before checking out.", currency, owed)
}

// writeFolioOwed answers a checkout refused because the folio still owes owed (owner decision
// 2026-11-14: no checkout unless settled).
func writeFolioOwed(w http.ResponseWriter, owed float64, currency string) {
	respondJSON(w, http.StatusConflict, map[string]any{
		"error":    "outstanding_balance",
		"message":  folioOwedMessage(owed, currency),
		"balance":  money.Round2(owed),
		"currency": currency,
	})
}

// checkOutLocked checks guest out. The caller holds the stay row lock in tx
// (payments.LockActiveStay), so no room charge, folio line or folio payment can change the
// balance while it is read here (review R-5). It refuses while anything is owed (owed > 0, done
// false) and keeps the conditional UPDATE, so a stay another request already checked out reports
// done false. The room goes to cleaning in the same transaction.
func checkOutLocked(ctx context.Context, tx *ent.Tx, guest *ent.RoomGuest, by uuid.UUID, now time.Time) (owed float64, done bool, err error) {
	owed, err = payments.FolioOutstanding(ctx, tx.Client(), guest)
	if err != nil {
		return 0, false, err
	}
	if owed > 0.009 {
		return owed, false, nil
	}
	affected, err := tx.RoomGuest.Update().
		Where(entroomguest.ID(guest.ID), entroomguest.StatusEQ(entroomguest.StatusActive)).
		SetStatus(entroomguest.StatusCheckedOut).
		SetCheckedOutBy(by).
		SetCheckedOutAt(now).
		Save(ctx)
	if err != nil || affected == 0 {
		return 0, false, err
	}
	if _, err := tx.Room.UpdateOneID(guest.RoomID).SetStatus(entroom.StatusCleaning).Save(ctx); err != nil {
		return 0, false, fmt.Errorf("room status: %w", err)
	}
	return 0, true, nil
}

// checkOutStay runs checkOutLocked in its own transaction under the stay lock (CheckOut and
// BatchCheckout). A stay that is no longer active reports done false.
func (h *HotelHandler) checkOutStay(ctx context.Context, tid, guestID, by uuid.UUID, now time.Time) (owed float64, done bool, err error) {
	tx, err := h.client.Tx(ctx)
	if err != nil {
		return 0, false, err
	}
	guest, err := payments.LockActiveStay(ctx, tx, tid, guestID)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	owed, done, err = checkOutLocked(ctx, tx, guest, by, now)
	if err != nil || !done {
		_ = tx.Rollback()
		return owed, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return 0, true, nil
}
