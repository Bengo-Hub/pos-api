package handlers

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"

	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
)

// outletPaymentDetails is what a customer needs to pay the business directly by M-Pesa: the same
// Till/Paybill/Pochi numbers the outlet prints on its receipts.
type outletPaymentDetails struct {
	MpesaTill         string `json:"mpesa_till,omitempty"`
	MpesaPaybill      string `json:"mpesa_paybill,omitempty"`
	MpesaAccountRef   string `json:"mpesa_account_reference,omitempty"`
	MpesaPochi        string `json:"mpesa_pochi,omitempty"`
	ManualMpesaOnline bool   `json:"manual_mpesa_online"`
}

// S2SOutletPaymentDetails handles GET /api/v1/s2s/{tenant}/outlets/{outletID}/payment-details.
// ordering-backend uses it to offer "Pay to our M-Pesa and enter the code" at checkout. The option
// is available when the outlet has an M-Pesa number configured and has not switched online manual
// M-Pesa off (outlet setting metadata "online_manual_mpesa" = false).
func (h *OnlineOrderHandler) S2SOutletPaymentDetails(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant"))
	if err != nil {
		jsonError(w, "invalid tenant", http.StatusBadRequest)
		return
	}
	outletID, err := uuid.Parse(chi.URLParam(r, "outletID"))
	if err != nil {
		jsonError(w, "invalid outlet id", http.StatusBadRequest)
		return
	}
	if ok, _ := h.db.Outlet.Query().Where(entoutlet.ID(outletID), entoutlet.TenantID(tid)).Exist(r.Context()); !ok {
		jsonOK(w, outletPaymentDetails{})
		return
	}
	setting, err := h.db.OutletSetting.Query().
		Where(entoutletsetting.OutletID(outletID)).
		Only(r.Context())
	if err != nil {
		jsonOK(w, outletPaymentDetails{})
		return
	}
	d := outletPaymentDetails{
		MpesaTill:       derefStr(setting.MpesaTill),
		MpesaPaybill:    derefStr(setting.MpesaPaybill),
		MpesaAccountRef: derefStr(setting.MpesaAccountReference),
		MpesaPochi:      derefStr(setting.MpesaPochi),
	}
	enabled := true
	if setting.Metadata != nil {
		if v, ok := setting.Metadata["online_manual_mpesa"].(bool); ok {
			enabled = v
		}
	}
	d.ManualMpesaOnline = enabled && (d.MpesaTill != "" || d.MpesaPaybill != "" || d.MpesaPochi != "")
	jsonOK(w, d)
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// mpesaCodePattern matches an M-Pesa confirmation code (10 upper-case letters/digits).
var mpesaCodePattern = regexp.MustCompile(`^[A-Z0-9]{10}$`)

// verifyPaymentRequest is the body of the verify-payment action.
type verifyPaymentRequest struct {
	// Reference is the M-Pesa code the cashier matched on the business's M-Pesa statement/SMS.
	// Optional when the customer already submitted it at checkout.
	Reference string `json:"reference"`
}

// VerifyPayment handles POST /{tenantID}/pos/online-orders/{orderID}/verify-payment.
// For an online order the customer paid by M-Pesa to the business's own Till/Paybill, the cashier
// checks the code against the M-Pesa message and confirms it here. ordering-backend (which owns the
// order) marks it paid and records the payment in treasury under that code; the POS card then
// shows it as paid so it can be handed over.
func (h *OnlineOrderHandler) VerifyPayment(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	oid, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid orderID", http.StatusBadRequest)
		return
	}
	var body verifyPaymentRequest
	_ = json.NewDecoder(r.Body).Decode(&body)

	order, err := h.db.POSOrder.Get(r.Context(), oid)
	if err != nil || order.TenantID != tid {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}
	meta := order.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	if prepaid, _ := meta["prepaid"].(bool); prepaid {
		jsonOK(w, order) // already paid/verified
		return
	}
	reference := strings.ToUpper(strings.TrimSpace(body.Reference))
	if reference == "" {
		reference, _ = meta["mpesa_code"].(string)
	}
	if !mpesaCodePattern.MatchString(reference) {
		jsonError(w, "enter the 10-character M-Pesa code from the payment message", http.StatusBadRequest)
		return
	}
	external := h.externalOrderID(r, oid)
	if external == "" {
		jsonError(w, "not an online order", http.StatusBadRequest)
		return
	}
	if h.rider == nil || h.rider.ordering == nil || !h.rider.ordering.Enabled() {
		jsonError(w, "ordering service not configured", http.StatusServiceUnavailable)
		return
	}
	slug := tenantSlugFromRequest(r)
	if slug == "" {
		jsonError(w, "tenant context required", http.StatusBadRequest)
		return
	}
	if err := h.rider.ordering.VerifyManualPayment(r.Context(), slug, external, reference); err != nil {
		h.log.Warn("verify online payment failed", zap.Error(err), zap.String("external_order_id", external))
		jsonError(w, "could not confirm the payment: "+err.Error(), http.StatusBadGateway)
		return
	}

	meta["prepaid"] = true
	meta["amount_due"] = 0.0
	meta["payment_status"] = "paid"
	meta["mpesa_code"] = reference
	meta["payment_verified_at"] = time.Now().Format(time.RFC3339)
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		meta["payment_verified_by"] = claims.Subject
	}
	updated, err := h.db.POSOrder.UpdateOneID(oid).SetMetadata(meta).Save(r.Context())
	if err != nil {
		h.log.Warn("verify online payment: local update failed", zap.Error(err))
		jsonOK(w, order)
		return
	}
	jsonOK(w, updated)
}
