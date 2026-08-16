package handlers

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entlayawaypayment "github.com/bengobox/pos-service/internal/ent/layawaypayment"
	entlayawayplan "github.com/bengobox/pos-service/internal/ent/layawayplan"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	"github.com/bengobox/pos-service/internal/modules/printing"
	"github.com/bengobox/pos-service/internal/modules/printing/layouts"
	"github.com/bengobox/pos-service/internal/modules/providerfooter"
)

// Layaway receipts — the deposit taken at create time and every instalment recorded afterwards
// happen BEFORE any POSOrder exists (one is only created at Complete), so the sale receipt
// endpoint can't print them. These endpoints render the exact same layouts.Receipt through the
// exact same renderers, sourced from the plan/payment rows instead of an order.

// buildLayawayReceipt resolves the outlet/settings/branding/provider-footer context, builds the
// canonical view and converts it to the JSON receipt payload — the single conversion point shared
// by the plan-level and payment-level layaway receipt endpoints.
func (h *LayawayHandler) buildLayawayReceipt(ctx context.Context, tid uuid.UUID, plan *ent.LayawayPlan, payment *ent.LayawayPayment, sequence int, servedBy string) (receiptResponse, receiptBrand) {
	outlet, _ := h.db.Outlet.Query().Where(entoutlet.ID(plan.OutletID)).Only(ctx)
	setting, _ := h.db.OutletSetting.Query().Where(entoutletsetting.OutletID(plan.OutletID)).Only(ctx)

	brand := tenantBranding(ctx, h.db, h.cache, h.authURL, tid)
	show := providerfooter.Resolve(ctx, h.db, tid)
	view := printing.BuildLayawayReceiptView(plan, payment, printing.LayawayReceiptOpts{
		Outlet:             outlet,
		Setting:            setting,
		TenantName:         brand.CompanyName,
		Currency:           resolveOutletCurrency(ctx, h.db, plan.OutletID),
		ServedBy:           servedBy,
		Sequence:           sequence,
		ShowProviderFooter: &show,
	})
	// Contact-line fallbacks + the opt-in email gate — identical to buildReceiptForOrder, so a
	// layaway slip and a sale receipt print the same contact block.
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

	return newReceiptResponse(view, layouts.Resolve(receiptFormatSetting(setting), view.UseCase)), brand
}

// loadLayawayPlan loads a tenant-scoped plan, writing the standard 400/404/500 response itself.
// Returns nil when it has already answered the request.
func (h *LayawayHandler) loadLayawayPlan(w http.ResponseWriter, r *http.Request) *ent.LayawayPlan {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return nil
	}
	planID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid layaway plan id", http.StatusBadRequest)
		return nil
	}
	plan, err := h.db.LayawayPlan.Query().
		Where(entlayawayplan.ID(planID), entlayawayplan.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "layaway plan not found", http.StatusNotFound)
			return nil
		}
		h.log.Error("layaway receipt: get plan failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return nil
	}
	return plan
}

// GetLayawayReceipt handles GET /{tenantID}/pos/layaways/{id}/receipt — the PLAN-level slip
// printed right after Create, acknowledging the opening deposit (Create records the deposit on
// the plan itself and writes no LayawayPayment row, so there is no payment id to address yet).
// ?format=html|pdf; default JSON.
func (h *LayawayHandler) GetLayawayReceipt(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	plan := h.loadLayawayPlan(w, r)
	if plan == nil {
		return
	}
	receipt, brand := h.buildLayawayReceipt(r.Context(), tid, plan, nil, 1, r.URL.Query().Get("served_by"))
	writeReceiptResponse(w, h.log, receipt, brand, receipt.ReceiptNumber, r.URL.Query().Get("format"), true)
}

// GetLayawayPaymentReceipt handles
// GET /{tenantID}/pos/layaways/{id}/payments/{paymentID}/receipt — the slip for ONE recorded
// instalment. ?format=html|pdf; default JSON.
func (h *LayawayHandler) GetLayawayPaymentReceipt(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	plan := h.loadLayawayPlan(w, r)
	if plan == nil {
		return
	}
	paymentID, err := uuid.Parse(chi.URLParam(r, "paymentID"))
	if err != nil {
		jsonError(w, "invalid payment id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	// Scoped to BOTH the plan and the tenant — a payment id from another plan/tenant must 404,
	// never render someone else's money on this plan's letterhead.
	payment, err := h.db.LayawayPayment.Query().
		Where(
			entlayawaypayment.ID(paymentID),
			entlayawaypayment.LayawayPlanID(plan.ID),
			entlayawaypayment.TenantID(tid),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "layaway payment not found", http.StatusNotFound)
			return
		}
		h.log.Error("layaway receipt: get payment failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Instalment ordinal: the deposit is #1, so the Nth recorded payment prints as #N+1.
	// Counted by paid_at (ties broken by id) so a receipt reprints with the same number forever.
	earlier, _ := h.db.LayawayPayment.Query().
		Where(
			entlayawaypayment.LayawayPlanID(plan.ID),
			entlayawaypayment.PaidAtLT(payment.PaidAt),
		).
		Count(ctx)
	sequence := earlier + 2

	receipt, brand := h.buildLayawayReceipt(ctx, tid, plan, payment, sequence, r.URL.Query().Get("served_by"))
	writeReceiptResponse(w, h.log, receipt, brand, receipt.ReceiptNumber, r.URL.Query().Get("format"), true)
}
