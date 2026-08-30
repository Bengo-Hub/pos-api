package saleedit

import (
	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

// EditLine is one line of the caller's DESIRED final state for the order — the server diffs
// this against the order's live lines itself (closing the class of bug where a client-side
// diff computed against a stale snapshot silently resubmitted a wrong/no-op edit — see
// feedback_edit_sale_inplace_not_reversal memory). LineID nil means a brand-new line.
type EditLine struct {
	LineID           *uuid.UUID
	CatalogItemID    uuid.UUID
	SKU              string
	Name             string
	Quantity         float64 // requested FINAL quantity for this line
	UnitPrice        float64 // requested FINAL unit price
	TaxCodeID        string
	PriceIncludesTax bool
	TaxRate          *float64
}

// EditSaleRequest is the single atomic input to Service.Edit — the caller sends the FULL
// desired line set (not a pre-diffed reductions/increases split); the orchestrator loads the
// live order and computes the diff itself.
type EditSaleRequest struct {
	OrderID     uuid.UUID
	Reason      string
	RequestedBy uuid.UUID
	TenantSlug  string
	Lines       []EditLine
	CrmContactID       *uuid.UUID
	CustomerIdentifier string
	CustomerName       string
	// IncreaseSettlement decides how a net INCREASE's incremental value is settled:
	//   "" (default)  — mirrors the order's OWN current on-account status: a cash sale's
	//                    top-up is collected as cash, a genuine credit sale's top-up is added
	//                    to what the customer already owes. Never a hardcoded default (see
	//                    applyInPlaceIncrease's own history — a cash sale's top-up used to
	//                    silently become AR debt against the customer's account regardless of
	//                    how the original sale was actually paid).
	//   "credit"      — explicitly bill the increment to the customer's account regardless of
	//                    the original tender (requires an identifiable customer).
	//   any other value (a tender type, e.g. "cash"/"mpesa_manual"/"card_manual") — collect the
	//    increment immediately against that tender instead of extending credit. This is the
	//    "collect the extra amount immediately" alternative the edit-sale error has always
	//    offered but never implemented.
	IncreaseSettlement string
	// ExternalReference is the collected tender's reference (M-Pesa code, cheque no.) when
	// IncreaseSettlement names a manual tender type — optional.
	ExternalReference string
}

// EditSaleResult is what Edit returns to the caller.
type EditSaleResult struct {
	OrderID               uuid.UUID  `json:"order_id"`
	SaleEditID            uuid.UUID  `json:"sale_edit_id"`
	Kind                  string     `json:"kind"` // reduction | increase | mixed
	Fiscalized            bool       `json:"fiscalized"`
	LinkedReversalID      *uuid.UUID `json:"linked_reversal_id,omitempty"`
	LinkedReturnID        *uuid.UUID `json:"linked_return_id,omitempty"`
	LinkedAddendumOrderID *uuid.UUID `json:"linked_addendum_order_id,omitempty"`
	// PriceOnlyLinesSkipped lists lines whose price changed with quantity unchanged — not yet
	// supported (mirrors the prior pos-ui client-side warn-and-drop behavior; the caller
	// should surface the same warning it already does).
	PriceOnlyLinesSkipped []uuid.UUID `json:"price_only_lines_skipped,omitempty"`
}

// lineDiff is the server-computed diff between an order's live lines and the caller's
// desired final state.
type lineDiff struct {
	removed  []*ent.POSOrderLine // whole line removed
	reduced  []reducedLine       // qty decreased
	increased []increasedLine    // qty increased on an EXISTING line
	added    []EditLine          // brand-new lines
	priceOnlySkipped []uuid.UUID
}

type reducedLine struct {
	Line  *ent.POSOrderLine
	ByQty float64 // amount to reduce by
}

type increasedLine struct {
	Line  *ent.POSOrderLine
	ByQty float64 // amount to add
	Req   EditLine
}

// diffLines computes the diff. liveLines should be the order's CURRENTLY ACTIVE lines
// (already-fully-voided lines excluded) — remaining quantity is Quantity-VoidedQty for a
// partially-reduced line, so a second edit diffs against what's actually still on the sale.
//
// returnedQty is the quantity of each line ALREADY removed via a completed, fiscalized
// POSReturn (see returnedQtyByLine) — necessary on top of VoidedQty because a fiscalized
// reduction deliberately never touches the order's own lines (the original order stays
// untouched; the reduction is visible only via the linked POSReturn — see the orchestrator's
// own doc comment). Without this, a SECOND fiscalized reduction on the same line re-diffed
// against the order's still-unchanged VoidedQty and silently re-removed the same quantity a
// second time. Found live 2026-08-05 on codevertex-demo: two consecutive fiscalized
// reductions on one line each created their own POSReturn for the same 2 units. Pass nil for
// a non-fiscalized-only caller (effectiveRemainingQty then degrades to remainingQty).
func diffLines(liveLines []*ent.POSOrderLine, requested []EditLine, returnedQty map[uuid.UUID]float64) lineDiff {
	byID := make(map[uuid.UUID]*ent.POSOrderLine, len(liveLines))
	for _, l := range liveLines {
		byID[l.ID] = l
	}
	seen := make(map[uuid.UUID]bool, len(requested))

	var d lineDiff
	for _, req := range requested {
		if req.LineID == nil {
			d.added = append(d.added, req)
			continue
		}
		seen[*req.LineID] = true
		live, ok := byID[*req.LineID]
		if !ok {
			continue // referenced a line that no longer exists/isn't active — ignore
		}
		remaining := effectiveRemainingQty(live, returnedQty)
		switch {
		case req.Quantity < remaining-0.009:
			d.reduced = append(d.reduced, reducedLine{Line: live, ByQty: round2(remaining - req.Quantity)})
		case req.Quantity > remaining+0.009:
			d.increased = append(d.increased, increasedLine{Line: live, ByQty: round2(req.Quantity - remaining), Req: req})
		case req.UnitPrice > 0 && absFloat(req.UnitPrice-live.UnitPrice) > 0.004:
			d.priceOnlySkipped = append(d.priceOnlySkipped, live.ID)
		}
	}
	for _, live := range liveLines {
		if !seen[live.ID] {
			d.removed = append(d.removed, live)
		}
	}
	return d
}

func remainingQty(l *ent.POSOrderLine) float64 {
	if l.VoidedQty == nil {
		return l.Quantity
	}
	return l.Quantity - *l.VoidedQty
}

// effectiveRemainingQty is remainingQty further reduced by whatever this line already lost
// to a completed fiscalized POSReturn (see diffLines' doc comment) — the correct baseline for
// any diff against a fiscalized order's line. Clamped at 0 (a line can't go negative even if
// returns somehow exceed the recorded VoidedQty-adjusted quantity).
func effectiveRemainingQty(l *ent.POSOrderLine, returnedQty map[uuid.UUID]float64) float64 {
	r := remainingQty(l) - returnedQty[l.ID]
	if r < 0 {
		r = 0
	}
	return r
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// classifyKind labels the diff for POSSaleEdit.Kind.
func classifyKind(d lineDiff) string {
	hasDecrease := len(d.removed) > 0 || len(d.reduced) > 0
	hasIncrease := len(d.added) > 0 || len(d.increased) > 0
	switch {
	case hasDecrease && hasIncrease:
		return "mixed"
	case hasDecrease:
		return "reduction"
	case hasIncrease:
		return "increase"
	default:
		return "price_only"
	}
}
