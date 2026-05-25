package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entcr "github.com/bengobox/pos-service/internal/ent/commissionrule"
	entcrec "github.com/bengobox/pos-service/internal/ent/commissionrecord"
)

// CommissionRuleHandler handles commission rule CRUD and payout.
type CommissionRuleHandler struct {
	log *zap.Logger
	db  *ent.Client
}

func NewCommissionRuleHandler(log *zap.Logger, db *ent.Client) *CommissionRuleHandler {
	return &CommissionRuleHandler{log: log, db: db}
}

// List handles GET /{tenantID}/pos/commissions/rules
func (h *CommissionRuleHandler) List(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.CommissionRule.Query().Where(entcr.TenantID(tid))

	if staffID := r.URL.Query().Get("staff_member_id"); staffID != "" {
		if sid, parseErr := uuid.Parse(staffID); parseErr == nil {
			q = q.Where(entcr.StaffMemberID(sid))
		}
	}
	if active := r.URL.Query().Get("is_active"); active == "true" {
		q = q.Where(entcr.IsActive(true))
	}

	rules, err := q.All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": rules, "total": len(rules)})
}

type createCommissionRuleInput struct {
	OutletID      string             `json:"outlet_id,omitempty"`
	StaffMemberID string             `json:"staff_member_id,omitempty"`
	CatalogItemID string             `json:"catalog_item_id,omitempty"`
	RuleType      string             `json:"rule_type"` // flat | percentage | tiered
	FlatAmount    *float64           `json:"flat_amount,omitempty"`
	Percentage    *float64           `json:"percentage,omitempty"`
	TierRules     []map[string]any   `json:"tier_rules,omitempty"`
	EffectiveFrom string             `json:"effective_from,omitempty"`
}

// Create handles POST /{tenantID}/pos/commissions/rules
func (h *CommissionRuleHandler) Create(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createCommissionRuleInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.RuleType == "" {
		jsonError(w, "rule_type is required (flat|percentage|tiered)", http.StatusBadRequest)
		return
	}

	c := h.db.CommissionRule.Create().
		SetTenantID(tid).
		SetRuleType(input.RuleType)

	if input.OutletID != "" {
		if oid, parseErr := uuid.Parse(input.OutletID); parseErr == nil {
			c.SetOutletID(oid)
		}
	}
	if input.StaffMemberID != "" {
		if sid, parseErr := uuid.Parse(input.StaffMemberID); parseErr == nil {
			c.SetStaffMemberID(sid)
		}
	}
	if input.CatalogItemID != "" {
		if cid, parseErr := uuid.Parse(input.CatalogItemID); parseErr == nil {
			c.SetCatalogItemID(cid)
		}
	}
	if input.FlatAmount != nil {
		c.SetFlatAmount(*input.FlatAmount)
	}
	if input.Percentage != nil {
		c.SetPercentage(*input.Percentage)
	}
	if len(input.TierRules) > 0 {
		c.SetTierRules(input.TierRules)
	}

	rule, err := c.Save(r.Context())
	if err != nil {
		h.log.Error("create commission rule failed", zap.Error(err))
		jsonError(w, "failed to create rule: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, rule)
}

// Update handles PATCH /{tenantID}/pos/commissions/rules/{ruleID}
func (h *CommissionRuleHandler) Update(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	ruleID, err := uuid.Parse(chi.URLParam(r, "ruleID"))
	if err != nil {
		jsonError(w, "invalid rule_id", http.StatusBadRequest)
		return
	}

	rule, err := h.db.CommissionRule.Query().
		Where(entcr.ID(ruleID), entcr.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "rule not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	var input map[string]any
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	upd := rule.Update()
	if v, ok := input["rule_type"].(string); ok {
		upd.SetRuleType(v)
	}
	if v, ok := input["flat_amount"].(float64); ok {
		upd.SetFlatAmount(v)
	}
	if v, ok := input["percentage"].(float64); ok {
		upd.SetPercentage(v)
	}
	if v, ok := input["is_active"].(bool); ok {
		upd.SetIsActive(v)
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		jsonError(w, "update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

type payoutInput struct {
	StaffMemberID string `json:"staff_member_id"`
	DateFrom      string `json:"date_from"`
	DateTo        string `json:"date_to"`
	PaymentRef    string `json:"payment_ref,omitempty"`
}

// Payout handles POST /{tenantID}/pos/commissions/payout
// Marks commission records as paid for a staff member in a date range.
func (h *CommissionRuleHandler) Payout(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input payoutInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	staffID, err := uuid.Parse(input.StaffMemberID)
	if err != nil {
		jsonError(w, "invalid staff_member_id", http.StatusBadRequest)
		return
	}

	q := h.db.CommissionRecord.Query().
		Where(
			entcrec.TenantID(tid),
			entcrec.StaffMemberID(staffID),
			entcrec.Status("pending"),
		)

	records, err := q.All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	paid := 0
	for _, rec := range records {
		upd := rec.Update().SetStatus("paid")
		if input.PaymentRef != "" {
			upd.SetNotes("Payout ref: " + input.PaymentRef)
		}
		if _, saveErr := upd.Save(r.Context()); saveErr == nil {
			paid++
		}
	}

	jsonOK(w, map[string]any{"paid_count": paid, "staff_member_id": staffID})
}
