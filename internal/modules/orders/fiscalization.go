package orders

import (
	"context"

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
// a locally-cached signal. Centralizes what was previously duplicated verbatim in
// reversals.stepEtimsCreditNote and saledelete.isFiscalized (both called
// treasuryClient.GetInvoiceByReference(tenantSlug, "pos_order", orderID) with the
// identical interpretation of "fiscalized" — a non-empty invoice ID). Returns the
// invoice id too, since most callers need it right after (e.g. to raise a credit note).
func IsFiscalized(ctx context.Context, treasuryClient *treasury.Client, tenantSlug string, orderID uuid.UUID) (bool, string, error) {
	if treasuryClient == nil {
		return false, "", nil
	}
	inv, err := treasuryClient.GetInvoiceByReference(ctx, tenantSlug, "pos_order", orderID.String())
	if err != nil || inv == nil || inv.ID == "" {
		return false, "", nil
	}
	return true, inv.ID, nil
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
	}
	return crmContactID, name, phone
}
