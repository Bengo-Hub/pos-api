package payments

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	"github.com/bengobox/pos-service/internal/ent/roomguest"
)

// ErrRoomChargeNoGuest is returned when the chosen room has no checked-in guest to charge.
var ErrRoomChargeNoGuest = errors.New("payments: no guest is checked in to that room")

// ErrRoomChargeWithComplimentary refuses the complimentary payment method and Charge to Room on
// the same sale (S2-20 review I-3): treasury books those two legs differently and the mix would
// count the room part as cash. Owner decision 2026-11-13: the way to give part of a room-charged
// bill free is to comp those items as a discount (the manual discount, with its reason and manager
// approval) and charge the net to the room; a whole-bill comp keeps the complimentary method. The
// HTTP layer answers 422 with this message, which pos-ui shows to the cashier as it is.
var ErrRoomChargeWithComplimentary = errors.New("This bill cannot be part complimentary and part charged to a room. To give some items free, comp them as a discount (manual discount with a reason and manager approval), then charge the rest to the room. Use Complimentary only to comp the whole bill.")

// roomChargeFolioSource marks, in RoomFolioItem.metadata, a folio line written by the till's
// room charge (recordRoomCharge). Only these lines were booked to the guest receivable at the
// till; a line posted by hand on the folio (minibar, laundry, or an API client's own
// pos_order_id) is revenue at checkout as before.
const roomChargeFolioSource = "pos_room_charge"

// roomChargeKeyMeta holds, on the folio line, the client's idempotency key for the room charge.
const roomChargeKeyMeta = "request_key"

// IsRoomChargeFolioLine reports a folio line the till's room charge wrote, whose amount treasury
// already holds on the guest receivable. Checkout clears it from there (pos_charge_amount).
func IsRoomChargeFolioLine(it *ent.RoomFolioItem) bool {
	if it == nil || it.PosOrderID == nil {
		return false
	}
	src, _ := it.Metadata["source"].(string)
	return src == roomChargeFolioSource
}

// findRoomCharge returns the till room-charge line this request already wrote, or nil. The key is
// the request, not the order (review I-5), so a bill split across two rooms, or a second charge
// for the rest, posts each part, and a hand-posted line never blocks a charge:
//   - with a client key, the source-marked line carrying that key;
//   - without one, the source-marked line for the same order, room and amount (any amount when
//     the request left it to the server).
//
// lines is the folio query to use (the plain client, or the transaction's).
func findRoomCharge(ctx context.Context, lines *ent.RoomFolioItemClient, req RecordPaymentRequest) (*ent.RoomFolioItem, error) {
	items, err := lines.Query().
		Where(roomfolioitem.TenantID(req.TenantID), roomfolioitem.PosOrderID(req.OrderID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if !IsRoomChargeFolioLine(it) {
			continue
		}
		if req.RoomChargeKey != "" {
			if k, _ := it.Metadata[roomChargeKeyMeta].(string); k == req.RoomChargeKey {
				return it, nil
			}
			continue
		}
		if it.RoomID != req.RoomID {
			continue
		}
		if req.Amount <= 0 || math.Abs(it.Amount-req.Amount) < 0.01 {
			return it, nil
		}
	}
	return nil, nil
}

// hasCompletedTender reports whether the order has a completed payment of method.
func hasCompletedTender(ctx context.Context, pays *ent.POSPaymentClient, orderID uuid.UUID, method string) (bool, error) {
	rows, err := pays.Query().
		Where(pospayment.OrderID(orderID), pospayment.Status(StatusCompleted)).
		All(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range rows {
		if m, _ := p.PaymentData["method"].(string); strings.EqualFold(m, method) {
			return true, nil
		}
	}
	return false, nil
}

// unsupportedLock reports SQLite's refusal of SELECT ... FOR UPDATE. SQLite only backs the unit
// tests and serializes writers itself, so the read goes unlocked there; Postgres takes the lock.
func unsupportedLock(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOR UPDATE/SHARE not supported")
}

// lockOrderRow takes the order row lock for the rest of tx.
func lockOrderRow(ctx context.Context, tx *ent.Tx, tenantID, orderID uuid.UUID) error {
	q := func() *ent.POSOrderQuery {
		return tx.POSOrder.Query().Where(posorder.ID(orderID), posorder.TenantID(tenantID))
	}
	_, err := q().ForUpdate().Only(ctx)
	if unsupportedLock(err) {
		_, err = q().Only(ctx)
	}
	return err
}

// lockActiveGuest loads the room's checked-in guest under a row lock for the rest of tx, so a
// checkout committing at the same moment cannot leave the charge on a closed stay (review M-2).
func lockActiveGuest(ctx context.Context, tx *ent.Tx, tenantID, roomID uuid.UUID) (*ent.RoomGuest, error) {
	return lockStay(ctx, tx, roomguest.TenantID(tenantID), roomguest.RoomID(roomID))
}

// LockActiveStay loads the checked-in stay guestID under a row lock for the rest of tx; a stay
// that is no longer active is not found. Every path that adds to a folio, takes a folio payment or
// checks a guest out holds this lock, so checkout reads a balance nothing can change under it
// (review R-5, M-1). Lock order everywhere: the order row first (when there is one), then the
// stay, never the reverse.
func LockActiveStay(ctx context.Context, tx *ent.Tx, tenantID, guestID uuid.UUID) (*ent.RoomGuest, error) {
	return lockStay(ctx, tx, roomguest.TenantID(tenantID), roomguest.ID(guestID))
}

func lockStay(ctx context.Context, tx *ent.Tx, where ...predicate.RoomGuest) (*ent.RoomGuest, error) {
	q := func() *ent.RoomGuestQuery {
		return tx.RoomGuest.Query().Where(append(where, roomguest.StatusEQ(roomguest.StatusActive))...)
	}
	g, err := q().ForUpdate().Only(ctx)
	if unsupportedLock(err) {
		g, err = q().Only(ctx)
	}
	return g, err
}

// recordRoomCharge settles (part of) a till sale by charging it to the guest staying in
// req.RoomID (S2-20). In ONE transaction, under the order and stay row locks, it writes the folio
// line (charge type restaurant, pos_order_id and the source marker set) and a completed
// room_charge payment, then completes the order like any settled sale. pos.sale.finalized then
// carries selling_scheme room_charge (or mixed), so treasury books Dr guest receivable / Cr
// revenue (+ VAT) once, and the folio payment at checkout clears that receivable instead of
// booking the revenue again.
//
// Idempotent per request (findRoomCharge): a retry returns the first result and writes nothing.
// A sale with a complimentary part is refused (ErrRoomChargeWithComplimentary). No treasury intent
// is created, as for an on-account sale: no money moved at the till.
func (s *Service) recordRoomCharge(ctx context.Context, order *ent.POSOrder, req RecordPaymentRequest, currency string) (*CreateIntentResult, error) {
	if req.RoomID == uuid.Nil {
		return nil, fmt.Errorf("payments: choose the room to charge")
	}

	completedReturns, _ := s.completedReturnsTotal(ctx, order.ID)
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: start transaction: %w", err)
	}
	rollback := func(e error) (*CreateIntentResult, error) {
		_ = tx.Rollback()
		return nil, e
	}
	if err := lockOrderRow(ctx, tx, req.TenantID, order.ID); err != nil {
		return rollback(fmt.Errorf("payments: lock order: %w", err))
	}
	// A racing retry that committed first wins; this call answers as it did.
	if prior, perr := findRoomCharge(ctx, tx.RoomFolioItem, req); perr != nil {
		return rollback(fmt.Errorf("payments: check room charge: %w", perr))
	} else if prior != nil {
		_ = tx.Rollback()
		s.completeOrderIfFullyPaid(ctx, order)
		return &CreateIntentResult{IsCash: true}, nil
	}
	if comp, cerr := hasCompletedTender(ctx, tx.POSPayment, order.ID, TenderComplimentary); cerr != nil {
		return rollback(fmt.Errorf("payments: check complimentary: %w", cerr))
	} else if comp {
		return rollback(ErrRoomChargeWithComplimentary)
	}

	// The same fresh outstanding check the cash path makes under the lock.
	rows, err := tx.POSPayment.Query().
		Where(pospayment.OrderID(order.ID), pospayment.Status(StatusCompleted)).
		All(ctx)
	if err != nil {
		return rollback(fmt.Errorf("payments: re-check outstanding: %w", err))
	}
	collected := 0.0
	for _, p := range rows {
		method, _ := p.PaymentData["method"].(string)
		if !strings.EqualFold(method, TenderOnAccount) {
			collected += p.Amount
		}
	}
	outstanding := order.TotalAmount - collected - completedReturns
	if outstanding <= 0.009 {
		return rollback(fmt.Errorf("payments: order %s is already fully paid", order.OrderNumber))
	}
	amount := req.Amount
	if amount <= 0 || amount > outstanding+0.01 {
		amount = outstanding
	}

	guest, err := lockActiveGuest(ctx, tx, req.TenantID, req.RoomID)
	if err != nil {
		if ent.IsNotFound(err) {
			return rollback(ErrRoomChargeNoGuest)
		}
		return rollback(fmt.Errorf("payments: find the room's guest: %w", err))
	}

	item, err := writeRoomCharge(ctx, tx, order, guest, amount, currency, req.RoomChargeKey, req.TenderID, fmt.Sprintf("POS bill %s", order.OrderNumber), nil)
	if err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("payments: commit room charge: %w", err)
	}

	s.completeOrderIfFullyPaid(ctx, order)
	if s.publisher != nil {
		if perr := s.publisher.PublishHotelFolioCharge(ctx, req.TenantID, map[string]any{
			"room_id":      guest.RoomID,
			"guest_id":     guest.ID,
			"item_id":      item.ID,
			"description":  item.Description,
			"amount":       amount,
			"charge_type":  string(roomfolioitem.ChargeTypeRestaurant),
			"pos_order_id": order.ID,
		}); perr != nil {
			s.log.Warn("room charge: folio charge event not published", zap.String("order_id", order.ID.String()), zap.Error(perr))
		}
	}
	return &CreateIntentResult{IsCash: true}, nil
}

// writeRoomCharge writes, in tx, one till room charge: the folio line on the guest's stay (charge
// type restaurant, the order id and the source marker, so checkout clears it from the guest
// receivable) and the completed room_charge payment that settles that part of the order. key is
// the request's idempotency key (findRoomCharge), extra is merged into the payment's data. The
// caller holds the order and stay locks.
func writeRoomCharge(ctx context.Context, tx *ent.Tx, order *ent.POSOrder, guest *ent.RoomGuest, amount float64, currency, key string, tenderID uuid.UUID, description string, extra map[string]any) (*ent.RoomFolioItem, error) {
	createdBy := order.UserID
	if order.ServedByUserID != nil && *order.ServedByUserID != uuid.Nil {
		createdBy = *order.ServedByUserID
	}
	lineMeta := map[string]any{"source": roomChargeFolioSource}
	if key != "" {
		lineMeta[roomChargeKeyMeta] = key
	}
	item, err := tx.RoomFolioItem.Create().
		SetTenantID(order.TenantID).
		SetRoomID(guest.RoomID).
		SetRoomGuestID(guest.ID).
		SetDescription(description).
		SetAmount(amount).
		SetCurrency(currency).
		SetChargeType(roomfolioitem.ChargeTypeRestaurant).
		SetCreatedBy(createdBy).
		SetPosOrderID(order.ID).
		SetMetadata(lineMeta).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("payments: post the room folio line: %w", err)
	}
	data := map[string]any{
		"method":        TenderRoomCharge,
		"room_id":       guest.RoomID.String(),
		"room_guest_id": guest.ID.String(),
		"folio_item_id": item.ID.String(),
	}
	for k, v := range extra {
		data[k] = v
	}
	// external_reference is the order number, as for an on-account sale, so the treasury intent
	// reconciler never mistakes the row for a cash payment still waiting for its intent.
	if _, err := tx.POSPayment.Create().
		SetOrderID(order.ID).
		SetTenderID(tenderID).
		SetAmount(amount).
		SetCurrency(currency).
		SetStatus(StatusCompleted).
		SetPaymentData(data).
		SetNillableExternalReference(nilIfEmpty(order.OrderNumber)).
		Save(ctx); err != nil {
		return nil, fmt.Errorf("payments: record room charge payment: %w", err)
	}
	return item, nil
}

// ErrRoomStayClosed refuses Charge to Room for a sale edit's extra amount when the sale was not
// charged to a room or its stay is already checked out (owner decision 2026-11-14). The HTTP layer
// answers 409 with this message; collect now and credit stay available.
var ErrRoomStayClosed = errors.New("This sale was not charged to a room that is still occupied, so the extra amount cannot go on the folio. Collect it now or put it on credit.")

// RoomChargeStayOpen returns the open stay a sale was charged to, or nil when the sale has no
// completed room charge or its stay is over. Edit Sale offers Charge to Room for an increase only
// then. A lookup failure is an error.
func RoomChargeStayOpen(ctx context.Context, client *ent.Client, tenantID, orderID uuid.UUID) (*ent.RoomGuest, error) {
	charged, err := hasCompletedTender(ctx, client.POSPayment, orderID, TenderRoomCharge)
	if err != nil || !charged {
		return nil, err
	}
	return OpenRoomStayForOrder(ctx, client, tenantID, orderID)
}

// ChargeSaleEditToRoom puts a sale edit's extra amount on the folio of the stay the sale was
// charged to, the same way the till's Charge to Room does (writeRoomCharge): a source-marked folio
// line, so checkout clears it from the guest receivable as POS share, and a completed room_charge
// payment on the order. It runs in one transaction under the order lock, then the stay lock. key
// makes it idempotent: a retry with the same key writes nothing. The ledger side is the caller's
// sale-edit GL posting with selling scheme room_charge (Dr guest receivable). A sale with no open
// room stay is refused with ErrRoomStayClosed.
func ChargeSaleEditToRoom(ctx context.Context, client *ent.Client, order *ent.POSOrder, amount float64, key string, tenderID uuid.UUID) error {
	if amount <= 0.009 {
		return nil
	}
	stay, err := RoomChargeStayOpen(ctx, client, order.TenantID, order.ID)
	if err != nil {
		return fmt.Errorf("payments: find the sale's room stay: %w", err)
	}
	if stay == nil {
		return ErrRoomStayClosed
	}
	tx, err := client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("payments: start transaction: %w", err)
	}
	rollback := func(e error) error {
		_ = tx.Rollback()
		return e
	}
	if err := lockOrderRow(ctx, tx, order.TenantID, order.ID); err != nil {
		return rollback(fmt.Errorf("payments: lock order: %w", err))
	}
	guest, err := LockActiveStay(ctx, tx, order.TenantID, stay.ID)
	if ent.IsNotFound(err) {
		return rollback(ErrRoomStayClosed)
	}
	if err != nil {
		return rollback(fmt.Errorf("payments: lock the room stay: %w", err))
	}
	prior, err := findRoomCharge(ctx, tx.RoomFolioItem, RecordPaymentRequest{TenantID: order.TenantID, OrderID: order.ID, RoomChargeKey: key})
	if err != nil {
		return rollback(fmt.Errorf("payments: check room charge: %w", err))
	}
	if prior != nil {
		return rollback(nil)
	}
	desc := fmt.Sprintf("POS bill %s (edit)", order.OrderNumber)
	if _, err := writeRoomCharge(ctx, tx, order, guest, amount, order.Currency, key, tenderID, desc, map[string]any{"sale_edit_increase": true}); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("payments: commit room charge: %w", err)
	}
	return nil
}
