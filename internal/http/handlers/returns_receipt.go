package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/modules/printing"
	"github.com/bengobox/pos-service/internal/modules/printing/layouts"
	"github.com/bengobox/pos-service/internal/modules/providerfooter"
)

// Refund/return receipts. A return is a customer-facing money document in its own right (goods
// back, cash/store-credit out) and until now had no printable form at all. It renders through the
// SAME layouts.Receipt + renderers as a sale — layouts.Receipt.IsReturn is what makes them print
// "REFUND TOTAL" and frame the document against the original order.
//
// Exchanges need nothing special: an exchange's replacement order is an ordinary fully-paid
// POSOrder, printable through the existing /orders/{orderID}/receipt endpoint. The frontend picks
// the endpoint by return_type; this one renders whatever the POSReturn holds.

// GetReturnReceipt handles GET /{tenantID}/pos/returns/{returnID}/receipt
// Query param ?format=pdf|html renders the refund receipt with the outlet's resolved layout;
// default returns the JSON receipt payload (identical shape to the sale receipt endpoint).
func (h *ReturnHandler) GetReturnReceipt(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	returnID, err := uuid.Parse(chi.URLParam(r, "returnID"))
	if err != nil {
		jsonError(w, "invalid return_id", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	ret, err := h.client.POSReturn.Query().
		Where(posreturn.ID(returnID), posreturn.TenantID(tid)).
		WithLines().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "return not found", http.StatusNotFound)
			return
		}
		h.log.Error("return receipt: query return", zap.Error(err))
		jsonError(w, "failed to get return", http.StatusInternalServerError)
		return
	}

	// The original sale supplies the "Return against Order #…" reference, the customer the refund
	// goes back to, and the currency the sale was priced in.
	var originalNumber, billTo, currency string
	if ret.OrderID != uuid.Nil {
		if o, oerr := h.client.POSOrder.Query().
			Where(entposorder.ID(ret.OrderID), entposorder.TenantID(tid)).
			Only(ctx); oerr == nil && o != nil {
			originalNumber = o.OrderNumber
			currency = o.Currency
			if o.CustomerName != nil {
				billTo = *o.CustomerName
			}
			if billTo == "" && o.CustomerPhone != nil {
				billTo = *o.CustomerPhone
			}
		}
	}
	if currency == "" {
		currency = resolveOutletCurrency(ctx, h.client, ret.OutletID)
	}

	outlet, _ := h.client.Outlet.Query().Where(entoutlet.ID(ret.OutletID)).Only(ctx)
	setting, _ := h.client.OutletSetting.Query().Where(entoutletsetting.OutletID(ret.OutletID)).Only(ctx)

	brand := tenantBranding(ctx, h.client, h.cache, h.authURL, tid)
	show := providerfooter.Resolve(ctx, h.client, tid)
	view := printing.BuildReturnReceiptView(ret, ret.Edges.Lines, printing.ReturnReceiptOpts{
		Outlet:              outlet,
		Setting:             setting,
		TenantName:          brand.CompanyName,
		OriginalOrderNumber: originalNumber,
		Currency:            currency,
		BillTo:              billTo,
		ServedBy:            r.URL.Query().Get("served_by"),
		ShowProviderFooter:  &show,
	})
	// Contact-line fallbacks + the opt-in email gate — identical to buildReceiptForOrder.
	if view.OutletPhones == "" {
		view.OutletPhones = brand.Phone
	}
	if view.OutletEmail == "" {
		view.OutletEmail = brand.Email
	}
	var settingMeta map[string]any
	if setting != nil {
		settingMeta = setting.Metadata
	}
	if !metaBoolDefault(settingMeta, "receipt_show_tenant_email", false) {
		view.OutletEmail = ""
	}
	view.ProviderFooter = printing.ResolveProviderFooter(ctx, h.cache, h.authURL)

	receipt := newReceiptResponse(view, layouts.Resolve(receiptFormatSetting(setting), view.UseCase))
	writeReceiptResponse(w, h.log, receipt, brand, ret.ReturnNumber, r.URL.Query().Get("format"), true)
}
