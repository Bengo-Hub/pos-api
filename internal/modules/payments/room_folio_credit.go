package payments

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/ent/posreversal"
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

// TreasuryRefundChannel is the channel sent to treasury for a POS refund channel. Since S2-20 a
// room charge is booked to the guest receivable at the till (selling_scheme room_charge), so a
// folio credit goes to treasury as room_folio and reduces that receivable: nothing leaves the
// drawer and no cash is booked out. Every other channel goes as it is.
func TreasuryRefundChannel(channel string) string {
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

// FolioCreditLimit is the most a refund of orderID may credit to the stay's folio (S2-20 review
// I-1): what the folio still owes, and no more than the order's room charge less the credits
// already made for its other returns and reversals. Only the room-charged part of the sale sits
// on the guest receivable, so a credit beyond it would drive the receivable negative and
// understate room revenue at checkout; the rest goes back through the cashier's channel.
// reference is this refund's own return or reversal number, left out of the earlier credits so a
// retry sees the same limit.
//
// The room-charged part is read from the folio lines the till's room charge wrote for the order
// on this stay (IsRoomChargeFolioLine), never from the room_charge payment rows: a reversal's
// stepPOSTotals nets those rows down before stepTreasuryGL asks for this limit (re-review R-1),
// while the folio lines keep what the guest was charged. Earlier credits for the order's other
// returns and reversals are what lowers the limit.
func FolioCreditLimit(ctx context.Context, client *ent.Client, guest *ent.RoomGuest, orderID uuid.UUID, reference string) float64 {
	limit := FolioOutstanding(ctx, client, guest)
	charged := 0.0
	lines, err := client.RoomFolioItem.Query().
		Where(
			roomfolioitem.TenantID(guest.TenantID),
			roomfolioitem.RoomGuestID(guest.ID),
			roomfolioitem.PosOrderID(orderID),
		).
		All(ctx)
	if err != nil {
		return 0
	}
	for _, it := range lines {
		if IsRoomChargeFolioLine(it) {
			charged += it.Amount
		}
	}
	sources := map[string]bool{}
	if ids, err := client.POSReturn.Query().Where(posreturn.OrderID(orderID)).IDs(ctx); err == nil {
		for _, id := range ids {
			sources[id.String()] = true
		}
	}
	if ids, err := client.POSReversal.Query().Where(posreversal.OrderID(orderID)).IDs(ctx); err == nil {
		for _, id := range ids {
			sources[id.String()] = true
		}
	}
	credits, err := client.RoomFolioPayment.Query().
		Where(
			roomfoliopayment.TenantID(guest.TenantID),
			roomfoliopayment.RoomGuestID(guest.ID),
			roomfoliopayment.Method(FolioReturnCreditMethod),
			roomfoliopayment.Status("completed"),
		).
		All(ctx)
	if err != nil {
		return 0
	}
	for _, c := range credits {
		if c.Reference == reference {
			continue
		}
		if src, _ := c.Metadata["source_id"].(string); sources[src] {
			charged -= c.Amount
		}
	}
	if charged < limit {
		limit = charged
	}
	if limit < 0.005 {
		return 0
	}
	return limit
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
