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
	q := func() *ent.RoomGuestQuery {
		return tx.RoomGuest.Query().
			Where(roomguest.TenantID(tenantID), roomguest.RoomID(roomID), roomguest.StatusEQ(roomguest.StatusActive))
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

	createdBy := order.UserID
	if order.ServedByUserID != nil && *order.ServedByUserID != uuid.Nil {
		createdBy = *order.ServedByUserID
	}
	lineMeta := map[string]any{"source": roomChargeFolioSource}
	if req.RoomChargeKey != "" {
		lineMeta[roomChargeKeyMeta] = req.RoomChargeKey
	}
	item, err := tx.RoomFolioItem.Create().
		SetTenantID(req.TenantID).
		SetRoomID(guest.RoomID).
		SetRoomGuestID(guest.ID).
		SetDescription(fmt.Sprintf("POS bill %s", order.OrderNumber)).
		SetAmount(amount).
		SetCurrency(currency).
		SetChargeType(roomfolioitem.ChargeTypeRestaurant).
		SetCreatedBy(createdBy).
		SetPosOrderID(order.ID).
		SetMetadata(lineMeta).
		Save(ctx)
	if err != nil {
		return rollback(fmt.Errorf("payments: post the room folio line: %w", err))
	}
	// external_reference is the order number, as for an on-account sale, so the treasury intent
	// reconciler never mistakes the row for a cash payment still waiting for its intent.
	if _, err := tx.POSPayment.Create().
		SetOrderID(order.ID).
		SetTenderID(req.TenderID).
		SetAmount(amount).
		SetCurrency(currency).
		SetStatus(StatusCompleted).
		SetPaymentData(map[string]any{
			"method":        TenderRoomCharge,
			"room_id":       guest.RoomID.String(),
			"room_guest_id": guest.ID.String(),
			"folio_item_id": item.ID.String(),
		}).
		SetNillableExternalReference(nilIfEmpty(order.OrderNumber)).
		Save(ctx); err != nil {
		return rollback(fmt.Errorf("payments: record room charge payment: %w", err))
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
