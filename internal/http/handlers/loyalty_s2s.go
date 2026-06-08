package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	entlp "github.com/bengobox/pos-service/internal/ent/loyaltyprogram"
)

// Loyalty S2S endpoints.
//
// pos-api is the source of truth for loyalty balances (keyed on tenant + customer_phone).
// Other backends (e.g. ordering-backend) earn/redeem against this service over S2S instead of
// keeping their own balance. These handlers are mounted under the internal /api/v1/s2s/{tenant}
// route group, which is guarded by the shared INTERNAL_SERVICE_KEY (X-API-Key) middleware.
//
// They reuse the same applyEarn/applyRedeem core as the staff-facing handlers so the earn/redeem
// logic lives in exactly one place; the only S2S-specific behaviour is resolving the account by
// (tenant, phone) — finding or creating it — rather than by account ID.

// s2sTenantUUID parses the {tenant} URL path parameter. S2S routes carry no auth/tenant-context
// middleware, so the tenant is taken directly from the path.
func s2sTenantUUID(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, "tenant"))
}

// findOrCreateAccountByPhone returns the tenant's loyalty account for the given phone, creating
// one (and linking it to the tenant's active program + async CRM contact) when none exists. This
// mirrors the CreateAccount handler's resolution logic so S2S earn/redeem can be call-and-forget.
func (h *LoyaltyHandler) findOrCreateAccountByPhone(ctx context.Context, tid uuid.UUID, phone, name string) (*ent.LoyaltyAccount, error) {
	phone = strings.TrimSpace(phone)
	if acc, err := h.db.LoyaltyAccount.Query().
		Where(entla.TenantID(tid), entla.CustomerPhone(phone)).
		First(ctx); err == nil && acc != nil {
		return acc, nil
	}

	if name == "" {
		name = phone // fall back to the phone as the display name when none is supplied
	}
	creator := h.db.LoyaltyAccount.Create().
		SetTenantID(tid).
		SetCustomerPhone(phone).
		SetCustomerName(name)
	// Auto-link to the tenant's active loyalty program when one exists.
	if prog, progErr := h.db.LoyaltyProgram.Query().
		Where(entlp.TenantID(tid), entlp.IsActive(true)).
		First(ctx); progErr == nil {
		creator = creator.SetProgramID(prog.ID)
	}
	acc, err := creator.Save(ctx)
	if err != nil {
		return nil, err
	}
	// Async: link to MarketFlow CRM so tier-upgrade events can find the contact (best-effort).
	if h.marketflow != nil && h.marketflow.Enabled() {
		go func(accID, tenantID uuid.UUID, p, n string) {
			crmID := h.marketflow.UpsertContactByPhone(context.Background(), tenantID, p, n)
			if crmID != uuid.Nil {
				if err := h.db.LoyaltyAccount.UpdateOneID(accID).SetCrmContactID(crmID).Exec(context.Background()); err != nil {
					h.log.Warn("loyalty s2s: failed to write crm_contact_id", zap.Error(err))
				}
			}
		}(acc.ID, tid, phone, name)
	}
	return acc, nil
}

// S2SEarn handles POST /api/v1/s2s/{tenant}/loyalty/earn.
// Body: {customer_phone, customer_name?, points, order_id?}. Finds-or-creates the account by
// (tenant, phone), credits the points, and returns the new balance.
func (h *LoyaltyHandler) S2SEarn(w http.ResponseWriter, r *http.Request) {
	tid, err := s2sTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant", http.StatusBadRequest)
		return
	}
	var body struct {
		CustomerPhone string  `json:"customer_phone"`
		CustomerName  string  `json:"customer_name"`
		Points        int     `json:"points"`
		OrderID       *string `json:"order_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.CustomerPhone) == "" {
		jsonError(w, "customer_phone is required", http.StatusBadRequest)
		return
	}
	if body.Points <= 0 {
		jsonError(w, "points must be positive", http.StatusBadRequest)
		return
	}
	acc, err := h.findOrCreateAccountByPhone(r.Context(), tid, body.CustomerPhone, body.CustomerName)
	if err != nil {
		h.log.Error("s2s earn: find-or-create account failed", zap.Error(err))
		jsonError(w, "failed to resolve loyalty account", http.StatusInternalServerError)
		return
	}
	newBalance, tx, err := h.applyEarn(r.Context(), tid, acc, body.Points, body.OrderID, "Earned via S2S (ordering)")
	if err != nil {
		h.log.Error("s2s earn: update failed", zap.Error(err))
		jsonError(w, "failed to credit points", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"account_id": acc.ID, "balance": newBalance, "transaction": tx})
}

// S2SRedeem handles POST /api/v1/s2s/{tenant}/loyalty/redeem.
// Body: {customer_phone, points, order_id?}. Finds-or-creates the account by (tenant, phone),
// validates a sufficient balance, debits the points, and returns the new balance.
func (h *LoyaltyHandler) S2SRedeem(w http.ResponseWriter, r *http.Request) {
	tid, err := s2sTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant", http.StatusBadRequest)
		return
	}
	var body struct {
		CustomerPhone string  `json:"customer_phone"`
		CustomerName  string  `json:"customer_name"`
		Points        int     `json:"points"`
		OrderID       *string `json:"order_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.CustomerPhone) == "" {
		jsonError(w, "customer_phone is required", http.StatusBadRequest)
		return
	}
	if body.Points <= 0 {
		jsonError(w, "points must be positive", http.StatusBadRequest)
		return
	}
	acc, err := h.findOrCreateAccountByPhone(r.Context(), tid, body.CustomerPhone, body.CustomerName)
	if err != nil {
		h.log.Error("s2s redeem: find-or-create account failed", zap.Error(err))
		jsonError(w, "failed to resolve loyalty account", http.StatusInternalServerError)
		return
	}
	if acc.PointsBalance < body.Points {
		jsonError(w, "insufficient points balance", http.StatusUnprocessableEntity)
		return
	}
	newBalance, tx, err := h.applyRedeem(r.Context(), tid, acc, body.Points, body.OrderID, "Redeemed via S2S (ordering)")
	if err != nil {
		h.log.Error("s2s redeem: update failed", zap.Error(err))
		jsonError(w, "failed to debit points", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"account_id": acc.ID, "balance": newBalance, "transaction": tx})
}

// S2SBalance handles GET /api/v1/s2s/{tenant}/loyalty/balance?phone=...
// Returns the current balance for the (tenant, phone) account; 0 when no account exists yet.
func (h *LoyaltyHandler) S2SBalance(w http.ResponseWriter, r *http.Request) {
	tid, err := s2sTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant", http.StatusBadRequest)
		return
	}
	phone := strings.TrimSpace(r.URL.Query().Get("phone"))
	if phone == "" {
		jsonError(w, "phone is required", http.StatusBadRequest)
		return
	}
	acc, err := h.db.LoyaltyAccount.Query().
		Where(entla.TenantID(tid), entla.CustomerPhone(phone)).
		First(r.Context())
	if err != nil || acc == nil {
		// No account yet — report a zero balance rather than 404 so callers can treat it uniformly.
		jsonOK(w, map[string]any{"customer_phone": phone, "balance": 0, "exists": false})
		return
	}
	jsonOK(w, map[string]any{
		"account_id":      acc.ID,
		"customer_phone":  acc.CustomerPhone,
		"balance":         acc.PointsBalance,
		"lifetime_points": acc.LifetimePoints,
		"exists":          true,
	})
}
