package handlers

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	entlp "github.com/bengobox/pos-service/internal/ent/loyaltyprogram"
	entlt "github.com/bengobox/pos-service/internal/ent/loyaltytransaction"
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

// ListAccounts handles GET /{tenantID}/pos/loyalty/accounts
func (h *LoyaltyHandler) ListAccounts(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	q := h.db.LoyaltyAccount.Query().Where(entla.TenantID(tid))
	if phone := r.URL.Query().Get("phone"); phone != "" {
		q = q.Where(entla.CustomerPhoneContainsFold(phone))
	}
	p := pagination.Parse(r)
	total, _ := q.Clone().Count(r.Context())
	accounts, err := q.Order(ent.Desc(entla.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list loyalty accounts failed", zap.Error(err))
		jsonError(w, "failed to list accounts", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(accounts, total, p))
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
		go func(accID uuid.UUID, tenantID uuid.UUID, phone, name string) {
			crmID := h.marketflow.UpsertContactByPhone(context.Background(), tenantID, phone, name)
			if crmID != uuid.Nil {
				if err := h.db.LoyaltyAccount.UpdateOneID(accID).
					SetCrmContactID(crmID).
					Exec(context.Background()); err != nil {
					h.log.Warn("loyalty: failed to write crm_contact_id", zap.Error(err))
				}
			}
		}(acc.ID, tid, body.CustomerPhone, body.CustomerName)
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
	jsonOK(w, map[string]any{"account": acc, "transactions": txns})
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
	newBalance := acc.PointsBalance + body.Points
	_, err = h.db.LoyaltyAccount.UpdateOneID(aid).
		SetPointsBalance(newBalance).
		SetLifetimePoints(acc.LifetimePoints + body.Points).
		Save(r.Context())
	if err != nil {
		h.log.Error("earn points update failed", zap.Error(err))
		jsonError(w, "failed to update account", http.StatusInternalServerError)
		return
	}
	txCreator := h.db.LoyaltyTransaction.Create().
		SetTenantID(tid).
		SetAccountID(aid).
		SetTypeField("earn").
		SetPoints(body.Points).
		SetBalanceAfter(newBalance)
	if body.OrderID != nil {
		if oid, err := uuid.Parse(*body.OrderID); err == nil {
			txCreator = txCreator.SetOrderID(oid)
		}
	}
	if body.Notes != "" {
		txCreator = txCreator.SetNotes(body.Notes)
	}
	tx, err := txCreator.Save(r.Context())
	if err != nil {
		h.log.Warn("earn: failed to create transaction record", zap.Error(err))
	}
	// Publish event for notifications-service (WhatsApp/SMS "You earned X pts" message).
	if h.publisher != nil {
		payload := map[string]any{
			"account_id":     aid.String(),
			"customer_phone": acc.CustomerPhone,
			"customer_name":  acc.CustomerName,
			"points_earned":  body.Points,
			"balance_after":  newBalance,
		}
		if body.OrderID != nil {
			payload["order_id"] = *body.OrderID
		}
		if pubErr := h.publisher.PublishLoyaltyPointsEarned(r.Context(), tid, payload); pubErr != nil {
			h.log.Warn("earn: failed to publish loyalty.points.earned event", zap.Error(pubErr))
		}
	}
	jsonOK(w, map[string]any{"balance": newBalance, "transaction": tx})
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
	newBalance := acc.PointsBalance - body.Points
	_, err = h.db.LoyaltyAccount.UpdateOneID(aid).
		SetPointsBalance(newBalance).
		Save(r.Context())
	if err != nil {
		h.log.Error("redeem points update failed", zap.Error(err))
		jsonError(w, "failed to update account", http.StatusInternalServerError)
		return
	}
	txCreator := h.db.LoyaltyTransaction.Create().
		SetTenantID(tid).
		SetAccountID(aid).
		SetTypeField("redeem").
		SetPoints(-body.Points).
		SetBalanceAfter(newBalance)
	if body.OrderID != nil {
		if oid, err := uuid.Parse(*body.OrderID); err == nil {
			txCreator = txCreator.SetOrderID(oid)
		}
	}
	if body.Notes != "" {
		txCreator = txCreator.SetNotes(body.Notes)
	}
	tx, err := txCreator.Save(r.Context())
	if err != nil {
		h.log.Warn("redeem: failed to create transaction record", zap.Error(err))
	}
	jsonOK(w, map[string]any{"balance": newBalance, "transaction": tx})
}
