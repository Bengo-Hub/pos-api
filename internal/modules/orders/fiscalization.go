package orders

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entposrefund "github.com/bengobox/pos-service/internal/ent/posrefund"
	entposreturn "github.com/bengobox/pos-service/internal/ent/posreturn"
	entposreversal "github.com/bengobox/pos-service/internal/ent/posreversal"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// IsFiscalized is THE per-order fiscalization check — was THIS specific order actually
// invoiced/transmitted to KRA (as opposed to whether the tenant is eTIMS-integrated at
// all, a separate, tenant-level question — see treasury.Client.GetTaxProfile().
// EtimsActivated for that). A live network round-trip to treasury, always current, never
// a locally-cached signal.
//
// Queries treasury's etims_invoices record (via GetEtimsFiscal, source="pos_sale") rather
// than the general invoices table (GetInvoiceByReference, reference_type="pos_order").
// Fixed 2026-08-05, found live against codevertex-demo: treasury-api stopped auto-creating
// an invoices-table row for POS sales on 2026-06-09 (customer receipts are now generated
// on demand only), but every fiscalization check built since then — this function
// (centralizing reversals.stepEtimsCreditNote and saledelete.isFiscalized, both identical)
// plus treasury-api's own S2SShredSaleLedger hard-delete guard — kept reading that
// now-empty signal, so a genuinely KRA-transmitted sale (confirmed via a live sandbox
// invoice, CU number present) always read as "not fiscalized." GetEtimsFiscal reads the
// transmission record eTIMS transmission itself actually writes, so it stays correct
// regardless of whether a companion Invoice document exists. Returns the invoice's KRA CU
// number in the second slot for logging/display; callers that need to raise a credit note
// must go through returns.Service (EnsureCreditNoteForReturn, self-sufficient — it does not
// require a pre-existing Invoice row), not treasury.Client.CreateCreditNote (which does).
func IsFiscalized(ctx context.Context, treasuryClient *treasury.Client, tenantSlug string, orderID uuid.UUID) (bool, string, error) {
	if treasuryClient == nil {
		return false, "", nil
	}
	fi, err := treasuryClient.GetEtimsFiscal(ctx, tenantSlug, orderID.String())
	if err != nil || fi == nil {
		return false, "", nil
	}
	return true, fi.CuInvoiceNo, nil
}

// HasCorrectionHistory reports whether an order already has ANY prior return, refund, or
// reversal on record — the guard saledelete.Delete uses to refuse acting on an
// already-corrected sale (Delete is for a plain, untouched sale; saleedit's Edit
// orchestrator deliberately allows repeat corrections instead — see HasFullReversal below,
// the narrower guard it uses).
func HasCorrectionHistory(ctx context.Context, client *ent.Client, tenantID, orderID uuid.UUID) (bool, error) {
	if ok, err := client.POSReturn.Query().Where(entposreturn.TenantID(tenantID), entposreturn.OrderID(orderID)).Exist(ctx); err != nil {
		return false, err
	} else if ok {
		return true, nil
	}
	if ok, err := client.POSRefund.Query().Where(entposrefund.OrderID(orderID)).Exist(ctx); err != nil {
		return false, err
	} else if ok {
		return true, nil
	}
	if ok, err := client.POSReversal.Query().Where(entposreversal.TenantID(tenantID), entposreversal.OrderID(orderID)).Exist(ctx); err != nil {
		return false, err
	} else if ok {
		return true, nil
	}
	return false, nil
}

// HasFullReversal reports whether a FULL-scope POSReversal already exists against this
// order — the narrower, correct guard for the rebuilt saleedit orchestrator (see its own
// doc comment): a full reversal is terminal (the order is already "refunded", nothing
// left to correct), but a prior PARTIAL reversal or a completed POSReturn is not — repeat
// edits over time are expected and safe now that the cumulative VoidedQty fix (§4) removed
// the one-shot-per-line limitation that used to make re-editing risky.
func HasFullReversal(ctx context.Context, client *ent.Client, tenantID, orderID uuid.UUID) (bool, error) {
	return client.POSReversal.Query().
		Where(entposreversal.TenantID(tenantID), entposreversal.OrderID(orderID), entposreversal.ScopeEQ(entposreversal.ScopeFull)).
		Exist(ctx)
}

// CorrectionHistoryRollup batch-computes HasCorrectionHistory for a set of orders — one query
// per correction type instead of N. Lets a list/summary endpoint tell pos-ui UPFRONT whether
// Delete Sale will actually succeed for a given row, instead of only learning after a 422 (the
// exact same "already has a return, refund, or reversal on record" guard HasCorrectionHistory
// enforces one order at a time — this is its batched sibling, same semantics).
func CorrectionHistoryRollup(ctx context.Context, client *ent.Client, tenantID uuid.UUID, orderIDs []uuid.UUID) map[uuid.UUID]bool {
	out := make(map[uuid.UUID]bool, len(orderIDs))
	if len(orderIDs) == 0 {
		return out
	}
	if rets, err := client.POSReturn.Query().Where(entposreturn.TenantID(tenantID), entposreturn.OrderIDIn(orderIDs...)).All(ctx); err == nil {
		for _, r := range rets {
			out[r.OrderID] = true
		}
	}
	if refs, err := client.POSRefund.Query().Where(entposrefund.OrderIDIn(orderIDs...)).All(ctx); err == nil {
		for _, r := range refs {
			out[r.OrderID] = true
		}
	}
	if revs, err := client.POSReversal.Query().Where(entposreversal.TenantID(tenantID), entposreversal.OrderIDIn(orderIDs...)).All(ctx); err == nil {
		for _, r := range revs {
			out[r.OrderID] = true
		}
	}
	return out
}

// FullReversalRollup batch-computes HasFullReversal for a set of orders — same one-query
// pattern as CorrectionHistoryRollup, but scoped to the narrower "terminal" condition Edit Sale
// actually enforces (see HasFullReversal's doc comment). Lets pos-ui disable the Edit Sale
// action only for a row it already knows will be refused, without over-blocking a sale that
// merely has a PARTIAL correction on record — Edit Sale explicitly still allows those.
func FullReversalRollup(ctx context.Context, client *ent.Client, tenantID uuid.UUID, orderIDs []uuid.UUID) map[uuid.UUID]bool {
	out := make(map[uuid.UUID]bool, len(orderIDs))
	if len(orderIDs) == 0 {
		return out
	}
	if revs, err := client.POSReversal.Query().
		Where(entposreversal.TenantID(tenantID), entposreversal.OrderIDIn(orderIDs...), entposreversal.ScopeEQ(entposreversal.ScopeFull)).
		All(ctx); err == nil {
		for _, r := range revs {
			out[r.OrderID] = true
		}
	}
	return out
}

// RequireIdentifiableCustomer resolves the AR key (a phone number, or "staff:<id>" for a staff
// credit sale) for a customer/name pair about to be booked on credit, refusing the shared
// "Walk-in Customer" ghost identity (blank phone, or a name matching that placeholder) — booking
// a debt against it would commingle unrelated debts onto one row that can never be collected or
// reconciled. Centralizes the guard payments.recordCreditSale already enforces for a NEW credit
// sale (service.go's walkInPhone/walkInName constants — that pattern, not this function, is what
// pos-api and marketflow both use to name the placeholder). Found missing 2026-08-05 from
// saleedit.applyInPlaceIncrease (a completed sale's in-place Edit-Sale increase), which let a
// true walk-in order (no name, no phone) get marked on_account with a receivable treasury could
// never attribute to any customer — the GL entry posted, but the customer-balance side silently
// no-op'd (treasury's PostSaleEditGL only bumps CustomerBalance when a CRM contact or identifier
// is present), leaving a stranded, uncollectable, un-reconcilable debt.
func RequireIdentifiableCustomer(name, phone string, isStaffCredit bool, staffID uuid.UUID) (arKey string, err error) {
	phone = strings.TrimSpace(phone)
	if isStaffCredit && phone == "" {
		return "staff:" + staffID.String(), nil
	}
	trimmedName := strings.TrimSpace(name)
	if phone == "" || strings.EqualFold(trimmedName, "walk-in customer") || strings.EqualFold(trimmedName, "walk in customer") {
		return "", fmt.Errorf("requires a customer with a phone number selected — attach one before adding value to this sale on credit, or collect the extra amount immediately instead")
	}
	return phone, nil
}

// ResolveOrderCustomer returns an order's buyer identity (CRM contact via the phone-matched
// loyalty account, name, phone) — the same linkage the returns/reversal flows forward to
// treasury for refund/AR-write-off attribution. Centralizes what was previously duplicated
// across reversals.Service.resolveOrderCustomer, handlers.ReturnHandler.
// resolveReturnCustomer, and saledelete (via the reversals wrapper) — three near-identical
// copies of the same loyalty-account lookup.
func ResolveOrderCustomer(ctx context.Context, client *ent.Client, tenantID, orderID uuid.UUID) (crmContactID, name, phone string) {
	order, err := client.POSOrder.Query().
		Where(entposorder.ID(orderID), entposorder.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return "", "", ""
	}
	if order.CustomerName != nil {
		name = *order.CustomerName
	}
	if order.CustomerPhone != nil {
		phone = *order.CustomerPhone
	}
	if phone != "" {
		if acc, accErr := client.LoyaltyAccount.Query().
			Where(entla.TenantID(tenantID), entla.CustomerPhone(phone)).
			First(ctx); accErr == nil && acc != nil && acc.CrmContactID != nil {
			crmContactID = acc.CrmContactID.String()
		}
	} else if sid, _ := order.Metadata["staff_member_id"].(string); sid != "" {
		// A staff-credit sale (an employee buying on their own staff account) has no real
		// customer phone by design — payments.staffCreditFromOrderParty's own convention
		// (payments/service.go's RecordCreditSale call, credit_settlement.go's
		// creditSettlementKey) keys its treasury AR debtor "staff:<staff_member_id>" instead of a
		// phone. Every caller of THIS function that posts an AR credit-back on a reduction
		// (reversals.stepTreasuryGL, returns, saledelete) used to see phone="" for such an order
		// and silently skip the whole AR side of a reduction — the staff member's real owed
		// balance never moved even though their sale's own line was voided/reduced. Duplicated
		// (not imported) because orders can't import payments without an import cycle.
		if id, perr := uuid.Parse(sid); perr == nil {
			phone = "staff:" + id.String()
		}
	}
	return crmContactID, name, phone
}
