package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	entlp "github.com/bengobox/pos-service/internal/ent/loyaltyprogram"
	entlt "github.com/bengobox/pos-service/internal/ent/loyaltytransaction"
	entpospayment "github.com/bengobox/pos-service/internal/ent/pospayment"
	entref "github.com/bengobox/pos-service/internal/ent/referral"
	enttender "github.com/bengobox/pos-service/internal/ent/tender"
	"github.com/bengobox/pos-service/internal/platform/events"
	"github.com/bengobox/pos-service/internal/platform/marketflow"
)

type LoyaltyHandler struct {
	log        *zap.Logger
	db         *ent.Client
	marketflow *marketflow.Client
	publisher  *events.Publisher
}

func NewLoyaltyHandler(log *zap.Logger, db *ent.Client, mf *marketflow.Client) *LoyaltyHandler {
	return &LoyaltyHandler{log: log, db: db, marketflow: mf}
}

// SetPublisher wires the NATS event publisher (optional — if nil, events are skipped).
func (h *LoyaltyHandler) SetPublisher(p *events.Publisher) {
	h.publisher = p
}

// ListPrograms handles GET /{tenantID}/pos/loyalty/programs
func (h *LoyaltyHandler) ListPrograms(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	p := pagination.Parse(r)
	baseQ := h.db.LoyaltyProgram.Query().Where(entlp.TenantID(tid))
	total, _ := baseQ.Clone().Count(r.Context())
	programs, err := baseQ.Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list loyalty programs failed", zap.Error(err))
		jsonError(w, "failed to list programs", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(programs, total, p))
}

// CreateProgram handles POST /{tenantID}/pos/loyalty/programs
func (h *LoyaltyHandler) CreateProgram(w http.ResponseWriter, r *http.Request) {
	if !requireConfigPermission(w, r, false) {
		return
	}
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var body struct {
		Name            string  `json:"name"`
		Description     string  `json:"description"`
		EarnRate        float64 `json:"earn_rate"`
		RedeemRate      float64 `json:"redeem_rate"`
		MinRedeemPoints int     `json:"min_redeem_points"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}
	creator := h.db.LoyaltyProgram.Create().
		SetTenantID(tid).
		SetName(body.Name)
	if body.Description != "" {
		creator = creator.SetDescription(body.Description)
	}
	if body.EarnRate > 0 {
		creator = creator.SetEarnRate(body.EarnRate)
	}
	if body.RedeemRate > 0 {
		creator = creator.SetRedeemRate(body.RedeemRate)
	}
	if body.MinRedeemPoints > 0 {
		creator = creator.SetMinRedeemPoints(body.MinRedeemPoints)
	}
	prog, err := creator.Save(r.Context())
	if err != nil {
		h.log.Error("create loyalty program failed", zap.Error(err))
		jsonError(w, "failed to create program", http.StatusInternalServerError)
		return
	}
	jsonOK(w, prog)
}

// UpdateProgram handles PUT /{tenantID}/pos/loyalty/programs/{programID}
func (h *LoyaltyHandler) UpdateProgram(w http.ResponseWriter, r *http.Request) {
	if !requireConfigPermission(w, r, false) {
		return
	}
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	pid, err := uuid.Parse(chi.URLParam(r, "programID"))
	if err != nil {
		jsonError(w, "invalid program_id", http.StatusBadRequest)
		return
	}
	prog, err := h.db.LoyaltyProgram.Get(r.Context(), pid)
	if err != nil || prog.TenantID != tid {
		jsonError(w, "program not found", http.StatusNotFound)
		return
	}
	var body struct {
		Name            *string  `json:"name"`
		Description     *string  `json:"description"`
		EarnRate        *float64 `json:"earn_rate"`
		RedeemRate      *float64 `json:"redeem_rate"`
		MinRedeemPoints *int     `json:"min_redeem_points"`
		IsActive        *bool    `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	upd := h.db.LoyaltyProgram.UpdateOneID(pid)
	if body.Name != nil {
		upd = upd.SetName(*body.Name)
	}
	if body.Description != nil {
		upd = upd.SetDescription(*body.Description)
	}
	if body.EarnRate != nil {
		upd = upd.SetEarnRate(*body.EarnRate)
	}
	if body.RedeemRate != nil {
		upd = upd.SetRedeemRate(*body.RedeemRate)
	}
	if body.MinRedeemPoints != nil {
		upd = upd.SetMinRedeemPoints(*body.MinRedeemPoints)
	}
	if body.IsActive != nil {
		upd = upd.SetIsActive(*body.IsActive)
	}
	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("update loyalty program failed", zap.Error(err))
		jsonError(w, "failed to update program", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// nationalSubscriberDigits strips non-digits and returns the last 9 digits — the Kenyan national
// subscriber number (without the leading 0 trunk prefix or +254 country code). Used to match loyalty
// phone searches regardless of how the number was entered/stored ("+254 792 548766" → "792548766").
func nationalSubscriberDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteByte(byte(r))
		}
	}
	d := b.String()
	if len(d) > 9 {
		return d[len(d)-9:]
	}
	return d
}

// loyaltyAccountMap is the explicit JSON shape for a loyalty account. ent tags every field
// omitempty, which silently DROPS zero-valued points_balance / lifetime_points from the response
// (a new account then arrives with no points field at all). This always includes them so the client
// reliably reads the balance.
func loyaltyAccountMap(acc *ent.LoyaltyAccount) map[string]any {
	m := map[string]any{
		"id":              acc.ID,
		"tenant_id":       acc.TenantID,
		"customer_phone":  acc.CustomerPhone,
		"customer_name":   acc.CustomerName,
		"customer_email":  acc.CustomerEmail,
		"points_balance":  acc.PointsBalance,
		"lifetime_points": acc.LifetimePoints,
		"created_at":      acc.CreatedAt,
		"updated_at":      acc.UpdatedAt,
	}
	if acc.ProgramID != nil {
		m["program_id"] = *acc.ProgramID
	}
	if acc.CrmContactID != nil {
		m["crm_contact_id"] = *acc.CrmContactID
	}
	if acc.CustomerID != nil {
		m["customer_id"] = *acc.CustomerID
	}
	return m
}

// ListAccounts handles GET /{tenantID}/pos/loyalty/accounts
func (h *LoyaltyHandler) ListAccounts(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	q := h.db.LoyaltyAccount.Query().Where(entla.TenantID(tid))
	if phone := r.URL.Query().Get("phone"); strings.TrimSpace(phone) != "" {
		// Match on the national subscriber number (last 9 digits) so the search is format-agnostic:
		// "+254 792 548766", "254792548766" and "0792548766" all normalize to "792548766", a substring
		// of every stored variant. Falls back to the raw input when it has no digits.
		needle := nationalSubscriberDigits(phone)
		if needle == "" {
			needle = strings.TrimSpace(phone)
		}
		q = q.Where(entla.CustomerPhoneContainsFold(needle))
	}
	// Name search (case-insensitive substring) so the customer picker can look up by name, not
	// only phone — previously the `name` param was silently ignored.
	if name := r.URL.Query().Get("name"); strings.TrimSpace(name) != "" {
		q = q.Where(entla.CustomerNameContainsFold(strings.TrimSpace(name)))
	}
	// Email search (case-insensitive substring) — QA req 2: customer lookup by name, phone OR email.
	if email := r.URL.Query().Get("email"); strings.TrimSpace(email) != "" {
		q = q.Where(entla.CustomerEmailContainsFold(strings.TrimSpace(email)))
	}
	p := pagination.Parse(r)
	total, _ := q.Clone().Count(r.Context())
	accounts, err := q.Order(ent.Desc(entla.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list loyalty accounts failed", zap.Error(err))
		jsonError(w, "failed to list accounts", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(accounts))
	for _, acc := range accounts {
		out = append(out, loyaltyAccountMap(acc))
	}

	// CRM merge — the customer master is MarketFlow, and most customers (e.g. a migrated legacy
	// base) have NO loyalty account yet. When the caller is SEARCHING (any of phone/name/email),
	// also search CRM contacts S2S and append the ones not already represented by a loyalty
	// account, so the POS customer picker finds every known customer, not only loyalty members.
	// CRM rows carry id:"" + source:"crm" (no points) — the till attaches them by phone/name and
	// can offer loyalty registration separately. First page only (CRM hits aren't paginated).
	phoneParam := strings.TrimSpace(r.URL.Query().Get("phone"))
	nameParam := strings.TrimSpace(r.URL.Query().Get("name"))
	emailParam := strings.TrimSpace(r.URL.Query().Get("email"))
	if h.marketflow != nil && h.marketflow.Enabled() && p.Offset == 0 && (phoneParam != "" || nameParam != "" || emailParam != "") {
		needle := nameParam
		if needle == "" {
			needle = emailParam
		}
		if phoneParam != "" {
			// Same format-agnostic phone needle as the loyalty query above.
			if d := nationalSubscriberDigits(phoneParam); d != "" {
				needle = d
			} else {
				needle = phoneParam
			}
		}
		// Keys already represented: crm contact ids + last-9-digit phones of the loyalty hits.
		seen := make(map[string]bool, len(accounts)*2)
		for _, acc := range accounts {
			if acc.CrmContactID != nil {
				seen["crm:"+acc.CrmContactID.String()] = true
			}
			if d := nationalSubscriberDigits(acc.CustomerPhone); d != "" {
				seen["ph:"+d] = true
			}
		}
		for _, c := range h.marketflow.SearchContacts(r.Context(), tid, needle, 10) {
			if c.ID != "" && seen["crm:"+c.ID] {
				continue
			}
			if d := nationalSubscriberDigits(c.Phone); d != "" && seen["ph:"+d] {
				continue
			}
			name := strings.TrimSpace(c.FirstName + " " + c.LastName)
			out = append(out, map[string]any{
				"id":              "",
				"tenant_id":       tid,
				"customer_name":   name,
				"customer_phone":  c.Phone,
				"customer_email":  c.Email,
				"crm_contact_id":  c.ID,
				"points_balance":  0,
				"lifetime_points": 0,
				"source":          "crm",
			})
			total++
		}
	}

	jsonOK(w, pagination.NewResponse(out, total, p))
}

// CreateAccount handles POST /{tenantID}/pos/loyalty/accounts
func (h *LoyaltyHandler) CreateAccount(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var body struct {
		CustomerPhone string  `json:"customer_phone"`
		CustomerName  string  `json:"customer_name"`
		CustomerEmail string  `json:"customer_email"`
		CustomerID    *string `json:"customer_id"`
		ProgramID     *string `json:"program_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.CustomerPhone == "" || body.CustomerName == "" {
		jsonError(w, "customer_phone and customer_name are required", http.StatusBadRequest)
		return
	}
	creator := h.db.LoyaltyAccount.Create().
		SetTenantID(tid).
		SetCustomerPhone(body.CustomerPhone).
		SetCustomerName(body.CustomerName)
	if e := strings.TrimSpace(body.CustomerEmail); e != "" {
		creator = creator.SetCustomerEmail(e)
	}
	if body.CustomerID != nil {
		if cid, err := uuid.Parse(*body.CustomerID); err == nil {
			creator = creator.SetCustomerID(cid)
		}
	}
	// Resolve program_id: use provided value or auto-select the tenant's active program.
	if body.ProgramID != nil {
		if pid, err := uuid.Parse(*body.ProgramID); err == nil {
			creator = creator.SetProgramID(pid)
		}
	} else {
		if prog, progErr := h.db.LoyaltyProgram.Query().
			Where(entlp.TenantID(tid), entlp.IsActive(true)).
			First(r.Context()); progErr == nil {
			creator = creator.SetProgramID(prog.ID)
		}
	}
	acc, err := creator.Save(r.Context())
	if err != nil {
		h.log.Error("create loyalty account failed", zap.Error(err))
		jsonError(w, "failed to create account", http.StatusInternalServerError)
		return
	}
	// Async: link to MarketFlow CRM so tier-upgrade events can find the contact.
	if h.marketflow != nil && h.marketflow.Enabled() {
		go func(accID uuid.UUID, tenantID uuid.UUID, phone, email, name string) {
			crmID := h.marketflow.UpsertContact(context.Background(), tenantID, phone, email, name)
			if crmID != uuid.Nil {
				if err := h.db.LoyaltyAccount.UpdateOneID(accID).
					SetCrmContactID(crmID).
					Exec(context.Background()); err != nil {
					h.log.Warn("loyalty: failed to write crm_contact_id", zap.Error(err))
				}
			}
		}(acc.ID, tid, body.CustomerPhone, strings.TrimSpace(body.CustomerEmail), body.CustomerName)
	}
	jsonOK(w, acc)
}

// GetAccount handles GET /{tenantID}/pos/loyalty/accounts/{accountID}
func (h *LoyaltyHandler) GetAccount(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	aid, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		jsonError(w, "invalid account_id", http.StatusBadRequest)
		return
	}
	acc, err := h.db.LoyaltyAccount.Get(r.Context(), aid)
	if err != nil || acc.TenantID != tid {
		jsonError(w, "account not found", http.StatusNotFound)
		return
	}
	txns, _ := h.db.LoyaltyTransaction.Query().
		Where(entlt.AccountID(aid)).
		Order(ent.Desc(entlt.FieldCreatedAt)).
		Limit(20).
		All(r.Context())
	jsonOK(w, map[string]any{"account": loyaltyAccountMap(acc), "transactions": txns})
}

// Earn handles POST /{tenantID}/pos/loyalty/accounts/{accountID}/earn
func (h *LoyaltyHandler) Earn(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	aid, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		jsonError(w, "invalid account_id", http.StatusBadRequest)
		return
	}
	acc, err := h.db.LoyaltyAccount.Get(r.Context(), aid)
	if err != nil || acc.TenantID != tid {
		jsonError(w, "account not found", http.StatusNotFound)
		return
	}
	var body struct {
		OrderID *string `json:"order_id"`
		Points  int     `json:"points"`
		Notes   string  `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Points <= 0 {
		jsonError(w, "points must be positive", http.StatusBadRequest)
		return
	}
	newBalance, tx, err := h.applyEarn(r.Context(), tid, acc, body.Points, body.OrderID, body.Notes)
	if err != nil {
		h.log.Error("earn points update failed", zap.Error(err))
		jsonError(w, "failed to update account", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"balance": newBalance, "transaction": tx})
}

// applyEarn is the shared core for crediting loyalty points to an account: it updates the
// points + lifetime balances, records an "earn" loyalty transaction, and publishes the
// loyalty.points.earned event. It is reused by both the staff-facing Earn handler and the
// S2S earn endpoint so the earn logic lives in exactly one place.
func (h *LoyaltyHandler) applyEarn(ctx context.Context, tid uuid.UUID, acc *ent.LoyaltyAccount, points int, orderID *string, notes string) (int, *ent.LoyaltyTransaction, error) {
	newBalance := acc.PointsBalance + points
	if _, err := h.db.LoyaltyAccount.UpdateOneID(acc.ID).
		SetPointsBalance(newBalance).
		SetLifetimePoints(acc.LifetimePoints + points).
		Save(ctx); err != nil {
		return 0, nil, err
	}
	txCreator := h.db.LoyaltyTransaction.Create().
		SetTenantID(tid).
		SetAccountID(acc.ID).
		SetTypeField("earn").
		SetPoints(points).
		SetBalanceAfter(newBalance)
	if orderID != nil {
		if oid, err := uuid.Parse(*orderID); err == nil {
			txCreator = txCreator.SetOrderID(oid)
		}
	}
	if notes != "" {
		txCreator = txCreator.SetNotes(notes)
	}
	tx, err := txCreator.Save(ctx)
	if err != nil {
		h.log.Warn("earn: failed to create transaction record", zap.Error(err))
	}
	// Publish event for notifications-service (WhatsApp/SMS "You earned X pts" message).
	if h.publisher != nil {
		payload := map[string]any{
			"account_id":     acc.ID.String(),
			"customer_phone": acc.CustomerPhone,
			"customer_name":  acc.CustomerName,
			"points_earned":  points,
			"balance_after":  newBalance,
		}
		if orderID != nil {
			payload["order_id"] = *orderID
		}
		if pubErr := h.publisher.PublishLoyaltyPointsEarned(ctx, tid, payload); pubErr != nil {
			h.log.Warn("earn: failed to publish loyalty.points.earned event", zap.Error(pubErr))
		}
	}
	return newBalance, tx, nil
}

// Redeem handles POST /{tenantID}/pos/loyalty/accounts/{accountID}/redeem
func (h *LoyaltyHandler) Redeem(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	aid, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		jsonError(w, "invalid account_id", http.StatusBadRequest)
		return
	}
	acc, err := h.db.LoyaltyAccount.Get(r.Context(), aid)
	if err != nil || acc.TenantID != tid {
		jsonError(w, "account not found", http.StatusNotFound)
		return
	}
	var body struct {
		OrderID *string `json:"order_id"`
		Points  int     `json:"points"`
		Notes   string  `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Points <= 0 {
		jsonError(w, "points must be positive", http.StatusBadRequest)
		return
	}
	if acc.PointsBalance < body.Points {
		jsonError(w, "insufficient points balance", http.StatusUnprocessableEntity)
		return
	}
	newBalance, tx, err := h.applyRedeem(r.Context(), tid, acc, body.Points, body.OrderID, body.Notes)
	if err != nil {
		h.log.Error("redeem points update failed", zap.Error(err))
		jsonError(w, "failed to update account", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"balance": newBalance, "transaction": tx})
}

// applyRedeem is the shared core for debiting loyalty points from an account: it updates the
// balance and records a "redeem" loyalty transaction. Callers MUST validate that the account
// has a sufficient balance (acc.PointsBalance >= points) before calling. Reused by both the
// staff-facing Redeem handler and the S2S redeem endpoint.
func (h *LoyaltyHandler) applyRedeem(ctx context.Context, tid uuid.UUID, acc *ent.LoyaltyAccount, points int, orderID *string, notes string) (int, *ent.LoyaltyTransaction, error) {
	newBalance := acc.PointsBalance - points
	if _, err := h.db.LoyaltyAccount.UpdateOneID(acc.ID).
		SetPointsBalance(newBalance).
		Save(ctx); err != nil {
		return 0, nil, err
	}
	txCreator := h.db.LoyaltyTransaction.Create().
		SetTenantID(tid).
		SetAccountID(acc.ID).
		SetTypeField("redeem").
		SetPoints(-points).
		SetBalanceAfter(newBalance)
	if orderID != nil {
		if oid, err := uuid.Parse(*orderID); err == nil {
			txCreator = txCreator.SetOrderID(oid)
		}
	}
	if notes != "" {
		txCreator = txCreator.SetNotes(notes)
	}
	tx, err := txCreator.Save(ctx)
	if err != nil {
		h.log.Warn("redeem: failed to create transaction record", zap.Error(err))
	}
	return newBalance, tx, nil
}

const defaultReferralBonus = 100

// referralCode returns a short, shareable, unique-ish referral code.
func referralCode() string {
	return "REF-" + strings.ToUpper(uuid.New().String()[:8])
}

// CreateReferral handles POST /{tenantID}/pos/loyalty/accounts/{accountID}/referrals.
// The account refers a friend by phone; the referrer earns bonus_points when the friend's first
// qualifying sale finalizes (see SaleFinalizedSubscriber.autoClaimReferral).
func (h *LoyaltyHandler) CreateReferral(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	aid, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		jsonError(w, "invalid account_id", http.StatusBadRequest)
		return
	}
	acc, err := h.db.LoyaltyAccount.Get(r.Context(), aid)
	if err != nil || acc.TenantID != tid {
		jsonError(w, "account not found", http.StatusNotFound)
		return
	}
	var body struct {
		ReferredPhone string `json:"referred_phone"`
		BonusPoints   int    `json:"bonus_points"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	phone := strings.TrimSpace(body.ReferredPhone)
	if phone == "" {
		jsonError(w, "referred_phone is required", http.StatusBadRequest)
		return
	}
	if phone == acc.CustomerPhone {
		jsonError(w, "cannot refer your own number", http.StatusBadRequest)
		return
	}
	bonus := body.BonusPoints
	if bonus <= 0 {
		bonus = defaultReferralBonus
	}
	// Idempotent: if a pending referral for this phone already exists, return it instead of duplicating.
	if existing, e := h.db.Referral.Query().
		Where(entref.TenantID(tid), entref.ReferredPhone(phone), entref.StatusEQ("pending")).
		First(r.Context()); e == nil && existing != nil {
		jsonOK(w, existing)
		return
	}
	ref, err := h.db.Referral.Create().
		SetTenantID(tid).
		SetReferrerAccountID(aid).
		SetReferredPhone(phone).
		SetCode(referralCode()).
		SetStatus("pending").
		SetBonusPoints(bonus).
		Save(r.Context())
	if err != nil {
		h.log.Error("create referral failed", zap.Error(err))
		jsonError(w, "failed to create referral", http.StatusInternalServerError)
		return
	}
	jsonOK(w, ref)
}

// ListReferrals handles GET /{tenantID}/pos/loyalty/accounts/{accountID}/referrals.
func (h *LoyaltyHandler) ListReferrals(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	aid, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		jsonError(w, "invalid account_id", http.StatusBadRequest)
		return
	}
	refs, err := h.db.Referral.Query().
		Where(entref.TenantID(tid), entref.ReferrerAccountID(aid)).
		Order(ent.Desc(entref.FieldCreatedAt)).
		All(r.Context())
	if err != nil {
		h.log.Error("list referrals failed", zap.Error(err))
		jsonError(w, "failed to list referrals", http.StatusInternalServerError)
		return
	}
	jsonOK(w, refs)
}

// ensureLoyaltyTender returns the tenant's "Loyalty Points" tender (type=loyalty), creating it on
// first use so loyalty redemptions can be recorded as a POSPayment like any other tender.
func (h *LoyaltyHandler) ensureLoyaltyTender(ctx context.Context, tid uuid.UUID) (*ent.Tender, error) {
	if t, err := h.db.Tender.Query().
		Where(enttender.TenantID(tid), enttender.Type("loyalty")).
		First(ctx); err == nil && t != nil {
		return t, nil
	}
	return h.db.Tender.Create().
		SetTenantID(tid).
		SetName("Loyalty Points").
		SetType("loyalty").
		SetIsActive(true).
		Save(ctx)
}

// RedeemToOrder handles POST /{tenantID}/pos/loyalty/accounts/{accountID}/redeem-to-order.
// "Pay with points": deducts points from the account and records a POSPayment on the tenant's
// loyalty tender for the equivalent currency value (points × program.redeem_rate), so the order's
// paid total increases like any other tender. Also records a redeem loyalty transaction.
func (h *LoyaltyHandler) RedeemToOrder(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	aid, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		jsonError(w, "invalid account_id", http.StatusBadRequest)
		return
	}
	acc, err := h.db.LoyaltyAccount.Get(r.Context(), aid)
	if err != nil || acc.TenantID != tid {
		jsonError(w, "account not found", http.StatusNotFound)
		return
	}
	var body struct {
		OrderID string `json:"order_id"`
		Points  int    `json:"points"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(body.OrderID)
	if err != nil {
		jsonError(w, "valid order_id is required", http.StatusBadRequest)
		return
	}
	if body.Points <= 0 {
		jsonError(w, "points must be positive", http.StatusBadRequest)
		return
	}
	if acc.PointsBalance < body.Points {
		jsonError(w, "insufficient points balance", http.StatusUnprocessableEntity)
		return
	}

	// Resolve redeem_rate + min_redeem from the account's program (fallback: tenant's active program).
	redeemRate, minRedeem := 0.01, 0
	var prog *ent.LoyaltyProgram
	if acc.ProgramID != nil {
		prog, _ = h.db.LoyaltyProgram.Get(r.Context(), *acc.ProgramID)
	}
	if prog == nil {
		prog, _ = h.db.LoyaltyProgram.Query().Where(entlp.TenantID(tid), entlp.IsActive(true)).First(r.Context())
	}
	if prog != nil {
		if prog.RedeemRate > 0 {
			redeemRate = prog.RedeemRate
		}
		minRedeem = prog.MinRedeemPoints
	}
	if minRedeem > 0 && body.Points < minRedeem {
		jsonError(w, fmt.Sprintf("minimum redemption is %d points", minRedeem), http.StatusUnprocessableEntity)
		return
	}
	amount := float64(body.Points) * redeemRate
	if amount <= 0 {
		jsonError(w, "redeemed amount must be positive", http.StatusUnprocessableEntity)
		return
	}

	tender, err := h.ensureLoyaltyTender(r.Context(), tid)
	if err != nil {
		h.log.Error("redeem-to-order: ensure tender failed", zap.Error(err))
		jsonError(w, "failed to resolve loyalty tender", http.StatusInternalServerError)
		return
	}

	newBalance := acc.PointsBalance - body.Points
	if _, err := h.db.LoyaltyAccount.UpdateOneID(aid).SetPointsBalance(newBalance).Save(r.Context()); err != nil {
		h.log.Error("redeem-to-order: balance update failed", zap.Error(err))
		jsonError(w, "failed to update account", http.StatusInternalServerError)
		return
	}
	tx, err := h.db.LoyaltyTransaction.Create().
		SetTenantID(tid).
		SetAccountID(aid).
		SetOrderID(orderID).
		SetTypeField("redeem").
		SetPoints(-body.Points).
		SetBalanceAfter(newBalance).
		SetNotes("Redeemed to order as tender").
		Save(r.Context())
	if err != nil {
		h.log.Warn("redeem-to-order: failed to create loyalty transaction", zap.Error(err))
	}

	pay, err := h.db.POSPayment.Create().
		SetOrderID(orderID).
		SetTenderID(tender.ID).
		SetAmount(amount).
		SetStatus("completed").
		SetPaymentData(map[string]any{"loyalty_account_id": aid.String(), "points_redeemed": body.Points}).
		Save(r.Context())
	if err != nil {
		h.log.Error("redeem-to-order: failed to record loyalty payment", zap.Error(err))
		jsonError(w, "failed to record loyalty payment", http.StatusInternalServerError)
		return
	}
	// Keep the order's stored paid_total (payment-status source of truth) in sync — this
	// handler records a completed payment outside the payments service's recompute path.
	if paid, aggErr := h.db.POSPayment.Query().
		Where(entpospayment.OrderID(orderID), entpospayment.Status("completed")).
		Aggregate(ent.Sum(entpospayment.FieldAmount)).
		Float64(r.Context()); aggErr == nil {
		_ = h.db.POSOrder.UpdateOneID(orderID).SetPaidTotal(paid).Exec(r.Context())
	}

	jsonOK(w, map[string]any{
		"balance":        newBalance,
		"amount_applied": amount,
		"currency":       "KES",
		"tender_id":      tender.ID,
		"payment":        pay,
		"transaction":    tx,
	})
}
