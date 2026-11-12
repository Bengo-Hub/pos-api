package payments

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	"github.com/bengobox/pos-service/internal/ent/roomfoliopayment"
	"github.com/bengobox/pos-service/internal/ent/roomguest"
)

// RefundChannelRoomFolio refunds a room-charged sale as a credit on the guest's room folio
// while the stay is open (owner decision 2026-11-12). Once the stay is checked out the refund
// goes through the cashier's chosen channel instead (cash by default).
const RefundChannelRoomFolio = "room_folio"

// FolioReturnCreditMethod is the RoomFolioPayment.method of a return credit: a completed folio
// "payment" that lowers what the guest owes at checkout. No money moves and no treasury intent
// is created for it.
const FolioReturnCreditMethod = "return_credit"

// TreasuryRefundChannel is the channel sent to treasury for a POS refund channel. A folio
// credit reverses a sale treasury booked as cash (room_charge settles as cash at the till, see
// treasuryMethodForImmediate), so treasury reverses it as cash too; nothing leaves the drawer.
func TreasuryRefundChannel(channel string) string {
	if channel == RefundChannelRoomFolio {
		return "cash"
	}
	return channel
}

// OpenRoomStayForOrder returns the open room stay a till sale was charged to (the folio item
// carrying the order id), or nil when the sale was not charged to a room or the stay is over.
func OpenRoomStayForOrder(ctx context.Context, client *ent.Client, tenantID, orderID uuid.UUID) *ent.RoomGuest {
	guest := RoomStayForOrder(ctx, client, tenantID, orderID)
	if guest == nil || guest.Status != roomguest.StatusActive {
		return nil
	}
	return guest
}

// RoomStayForOrder returns the room stay a till sale was charged to, open or closed, or nil. A
// retried reversal uses it to find a credit it already made after the stay closed.
func RoomStayForOrder(ctx context.Context, client *ent.Client, tenantID, orderID uuid.UUID) *ent.RoomGuest {
	item, err := client.RoomFolioItem.Query().
		Where(roomfolioitem.TenantID(tenantID), roomfolioitem.PosOrderID(orderID)).
		Order(ent.Desc(roomfolioitem.FieldCreatedAt)).
		First(ctx)
	if err != nil {
		return nil
	}
	guest, err := client.RoomGuest.Query().
		Where(roomguest.ID(item.RoomGuestID), roomguest.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil
	}
	return guest
}

// CreditRoomFolio records a return credit of amount on the guest's folio. It is idempotent on
// (tenant, stay, reference): a second call with the same reference records nothing and reports
// created=false. reference is the return or reversal number; sourceID its id.
func CreditRoomFolio(ctx context.Context, client *ent.Client, guest *ent.RoomGuest, amount float64, reference string, sourceID, by uuid.UUID) (bool, error) {
	if amount <= 0.009 {
		return false, errors.New("payments: a folio credit must be positive")
	}
	exists, err := client.RoomFolioPayment.Query().
		Where(
			roomfoliopayment.TenantID(guest.TenantID),
			roomfoliopayment.RoomGuestID(guest.ID),
			roomfoliopayment.Method(FolioReturnCreditMethod),
			roomfoliopayment.Reference(reference),
		).
		Exist(ctx)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	create := client.RoomFolioPayment.Create().
		SetTenantID(guest.TenantID).
		SetRoomID(guest.RoomID).
		SetRoomGuestID(guest.ID).
		SetAmount(amount).
		SetMethod(FolioReturnCreditMethod).
		SetReference(reference).
		SetStatus("completed").
		SetMetadata(map[string]any{"source_id": sourceID.String(), "kind": "pos_return_credit"})
	if by != uuid.Nil {
		create = create.SetRecordedBy(by)
	}
	if _, err := create.Save(ctx); err != nil {
		// A concurrent writer recorded the same credit first: the partial unique index
		// roomfoliopayment_return_credit_ref (tenant, stay, reference WHERE method =
		// 'return_credit') turns the race into a no-op.
		if ent.IsConstraintError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ExistingFolioCredit is the return credit already recorded on the stay under reference, or 0.
// A retried reversal step uses it so its own notes report the credit that was actually made.
func ExistingFolioCredit(ctx context.Context, client *ent.Client, guest *ent.RoomGuest, reference string) float64 {
	p, err := client.RoomFolioPayment.Query().
		Where(
			roomfoliopayment.TenantID(guest.TenantID),
			roomfoliopayment.RoomGuestID(guest.ID),
			roomfoliopayment.Method(FolioReturnCreditMethod),
			roomfoliopayment.Reference(reference),
		).
		First(ctx)
	if err != nil {
		return 0
	}
	return p.Amount
}

// FolioOutstanding is what the stay's folio still owes: its charges less its completed
// payments, never below zero. A return credit is capped at this, so a folio the guest already
// settled is never driven negative (the rest is refunded through the cashier's channel).
func FolioOutstanding(ctx context.Context, client *ent.Client, guest *ent.RoomGuest) float64 {
	items, err := client.RoomFolioItem.Query().
		Where(roomfolioitem.TenantID(guest.TenantID), roomfolioitem.RoomGuestID(guest.ID)).
		All(ctx)
	if err != nil {
		return 0
	}
	pays, err := client.RoomFolioPayment.Query().
		Where(
			roomfoliopayment.TenantID(guest.TenantID),
			roomfoliopayment.RoomGuestID(guest.ID),
			roomfoliopayment.Status("completed"),
		).
		All(ctx)
	if err != nil {
		return 0
	}
	owed := 0.0
	for _, it := range items {
		owed += it.Amount
	}
	for _, p := range pays {
		owed -= p.Amount
	}
	if owed < 0.005 {
		return 0
	}
	return owed
}

// SplitFolioCredit splits a refund of amount for a room-charged sale: the part the open stay's
// folio still owes is credited there, and the rest is refunded through the cashier's channel.
func SplitFolioCredit(amount, outstanding float64) (folio, rest float64) {
	if amount <= 0 {
		return 0, 0
	}
	folio = amount
	if outstanding < folio {
		folio = outstanding
	}
	if folio < 0.005 {
		folio = 0
	}
	return folio, amount - folio
}
