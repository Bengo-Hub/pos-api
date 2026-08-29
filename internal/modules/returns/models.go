package returns

import (
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent/posreturn"
)

// LineInput is one line of a return/exchange request.
type LineInput struct {
	OrderLineID uuid.UUID `json:"order_line_id"`
	// CatalogItemID identifies a replacement item on an EXCHANGE (exchange_lines) — return
	// lines reference the original order line instead.
	CatalogItemID uuid.UUID `json:"catalog_item_id,omitempty"`
	SKU           string    `json:"sku"`
	Name          string    `json:"name"`
	Quantity      float64   `json:"quantity"`
	UnitPrice     float64   `json:"unit_price"`
	TotalPrice    float64   `json:"total_price"`
	Reason        string    `json:"reason"`
	// Per-line tax for EXCHANGE replacement lines (as priced in the catalog), so the
	// replacement order's payable equals the delta the cashier quoted.
	TaxCodeID        string   `json:"tax_code_id,omitempty"`
	PriceIncludesTax bool     `json:"price_includes_tax,omitempty"`
	TaxRate          *float64 `json:"tax_rate,omitempty"`
}

// CreateReturnRequest describes a new return/exchange to initiate against an order.
type CreateReturnRequest struct {
	OrderID       uuid.UUID
	OutletID      uuid.UUID
	ReturnType    string // refund | exchange | store_credit
	Reason        string
	ReasonCode    string // changed_mind | defective | damaged | wrong_item | expired | other
	RefundChannel string // cash | mpesa | bank | cheque | store_credit | offset_invoice
	Lines         []LineInput
	RequestedBy   uuid.UUID
	// ReturnDate optionally backdates the return's recorded date (e.g. paperwork processed a
	// day after the customer actually brought the item back). Nil defaults to now, same as
	// before this field existed.
	ReturnDate *time.Time
	// BypassGuards skips the return-window-age check and the per-item is_returnable check —
	// for the admin Edit-Sale caller only (an authorized correction of a data-entry error is
	// not a customer physically returning goods). Never true for the customer-facing HTTP path.
	BypassGuards bool
}

// ApproveReturnRequest is the decision-only approve/reject step.
type ApproveReturnRequest struct {
	Action        string // approve | reject
	Notes         string
	RefundChannel string // optional override
	ApproverID    uuid.UUID
}

// CompleteReturnRequest is the final fulfilment step.
type CompleteReturnRequest struct {
	Notes         string
	RefundChannel string
	ExchangeLines []LineInput
	CompletedBy   uuid.UUID
}

// ExchangeResult reports the replacement order + money split back to the caller.
type ExchangeResult struct {
	OrderID          uuid.UUID `json:"order_id"`
	OrderNumber      string    `json:"order_number"`
	ReplacementTotal float64   `json:"replacement_total"`
	ExchangeCredit   float64   `json:"exchange_credit"`
	// AmountPayable is what the customer still owes on the replacement order (dearer swap);
	// the till collects it through the normal payment flow.
	AmountPayable float64 `json:"amount_payable"`
	// Leftover is the value still owed TO the customer (cheaper swap); CompleteReturn
	// settles it in treasury via the chosen refund channel.
	Leftover float64 `json:"leftover_refund"`
}

// AutoCompleteRequest is CreateAndAutoComplete's input — a reduction/removal on a fiscalized
// order, expressed as a return the caller wants created, approved and completed in one shot.
type AutoCompleteRequest struct {
	OrderID     uuid.UUID
	OutletID    uuid.UUID
	Reason      string
	ReasonCode  string
	Lines       []LineInput
	RequestedBy uuid.UUID
	// Source is stamped into the return's metadata (e.g. "edit_sale") so pos-ui can badge it
	// distinctly from a customer-initiated return in the Returns list.
	Source string
}

// refundChannelPtr converts a refund_channel string to a *posreturn.RefundChannel for the
// Nillable setter. Returns nil for empty/invalid input so an unset channel stays NULL.
func refundChannelPtr(s string) *posreturn.RefundChannel {
	switch posreturn.RefundChannel(s) {
	case posreturn.RefundChannelCash, posreturn.RefundChannelMpesa,
		posreturn.RefundChannelBank, posreturn.RefundChannelCheque,
		posreturn.RefundChannelStoreCredit, posreturn.RefundChannelOffsetInvoice:
		rc := posreturn.RefundChannel(s)
		return &rc
	}
	return nil
}

// reasonCodePtr converts a reason_code string to a *posreturn.ReasonCode for SetNillableReasonCode.
// Returns nil if the string is empty or not a valid enum value.
func reasonCodePtr(s string) *posreturn.ReasonCode {
	switch posreturn.ReasonCode(s) {
	case posreturn.ReasonCodeChangedMind, posreturn.ReasonCodeDefective,
		posreturn.ReasonCodeDamaged, posreturn.ReasonCodeWrongItem,
		posreturn.ReasonCodeExpired, posreturn.ReasonCodeOther:
		rc := posreturn.ReasonCode(s)
		return &rc
	}
	return nil
}

// cloneReturnMetadata returns a shallow copy of a return's metadata map so callers can add keys
// without mutating the loaded entity's map in place.
func cloneReturnMetadata(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src)+2)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
