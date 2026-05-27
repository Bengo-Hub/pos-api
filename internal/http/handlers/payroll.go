package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	entadv "github.com/bengobox/pos-service/internal/ent/staffadvance"
	entpay "github.com/bengobox/pos-service/internal/ent/staffpayroll"
	entpayl "github.com/bengobox/pos-service/internal/ent/staffpayrollline"
	"github.com/bengobox/pos-service/internal/ent/staffmember"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// PayrollHandler handles staff advance and payroll endpoints.
type PayrollHandler struct {
	log            *zap.Logger
	db             *ent.Client
	treasuryClient *treasury.Client
}

func NewPayrollHandler(log *zap.Logger, db *ent.Client, treasuryClient *treasury.Client) *PayrollHandler {
	return &PayrollHandler{log: log, db: db, treasuryClient: treasuryClient}
}

// CreateAdvance handles POST /{tenantID}/pos/staff/{staffID}/advances
func (h *PayrollHandler) CreateAdvance(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	staffID, err := uuid.Parse(chi.URLParam(r, "staffID"))
	if err != nil {
		jsonError(w, "invalid staff_id", http.StatusBadRequest)
		return
	}

	var input struct {
		Amount           float64 `json:"amount"`
		Reason           string  `json:"reason"`
		RepaymentMonths  int     `json:"repayment_months"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Amount <= 0 {
		jsonError(w, "amount is required and must be positive", http.StatusBadRequest)
		return
	}
	if input.RepaymentMonths <= 0 {
		input.RepaymentMonths = 1
	}

	// Verify staff belongs to this tenant
	if _, err := h.db.StaffMember.Query().
		Where(staffmember.ID(staffID), staffmember.TenantID(tid)).
		Only(r.Context()); err != nil {
		jsonError(w, "staff member not found", http.StatusNotFound)
		return
	}

	var approverID *uuid.UUID
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims.Subject != "" {
		id, _ := uuid.Parse(claims.Subject)
		approverID = &id
	}

	creator := h.db.StaffAdvance.Create().
		SetTenantID(tid).
		SetStaffID(staffID).
		SetAmount(input.Amount).
		SetReason(input.Reason).
		SetRepaymentMonths(input.RepaymentMonths).
		SetStatus("active")
	if approverID != nil {
		creator = creator.SetApprovedBy(*approverID).SetApprovedAt(time.Now())
	}

	adv, err := creator.Save(r.Context())
	if err != nil {
		h.log.Error("create advance failed", zap.Error(err))
		jsonError(w, "failed to create advance", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, adv)
}

// ListAdvances handles GET /{tenantID}/pos/staff/{staffID}/advances
func (h *PayrollHandler) ListAdvances(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	staffID, err := uuid.Parse(chi.URLParam(r, "staffID"))
	if err != nil {
		jsonError(w, "invalid staff_id", http.StatusBadRequest)
		return
	}

	p := pagination.Parse(r)
	baseQ := h.db.StaffAdvance.Query().Where(entadv.TenantID(tid), entadv.StaffID(staffID))
	total, _ := baseQ.Clone().Count(r.Context())
	advances, err := baseQ.Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		jsonError(w, "failed to list advances", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(advances, total, p))
}

// GeneratePayroll handles POST /{tenantID}/pos/payroll/generate
func (h *PayrollHandler) GeneratePayroll(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input struct {
		StaffID     string `json:"staff_id"`
		PeriodStart string `json:"period_start"` // YYYY-MM-DD
		PeriodEnd   string `json:"period_end"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	staffID, err := uuid.Parse(input.StaffID)
	if err != nil {
		jsonError(w, "invalid staff_id", http.StatusBadRequest)
		return
	}
	periodStart, err := time.Parse("2006-01-02", input.PeriodStart)
	if err != nil {
		jsonError(w, "period_start must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	periodEnd, err := time.Parse("2006-01-02", input.PeriodEnd)
	if err != nil {
		jsonError(w, "period_end must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}

	staff, err := h.db.StaffMember.Query().
		Where(staffmember.ID(staffID), staffmember.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "staff member not found", http.StatusNotFound)
		return
	}

	// Compute gross salary for the period
	var gross float64
	days := periodEnd.Sub(periodStart).Hours() / 24
	switch {
	case staff.MonthlySalary != nil && *staff.MonthlySalary > 0:
		gross = *staff.MonthlySalary
	case staff.DailyRate != nil && *staff.DailyRate > 0:
		gross = *staff.DailyRate * days
	case staff.HourlyRate != nil && *staff.HourlyRate > 0:
		gross = *staff.HourlyRate * days * 8
	}

	// Collect active advance deductions for this period
	advances, _ := h.db.StaffAdvance.Query().
		Where(entadv.TenantID(tid), entadv.StaffID(staffID), entadv.StatusEQ("active")).
		All(r.Context())

	var totalDeductions float64
	type lineInput struct {
		lineType    entpayl.LineType
		description string
		amount      float64
		advanceID   *uuid.UUID
	}
	var lines []lineInput
	lines = append(lines, lineInput{lineType: entpayl.LineTypeSalary, description: fmt.Sprintf("Salary %s - %s", input.PeriodStart, input.PeriodEnd), amount: gross})

	for _, adv := range advances {
		monthlyInstalment := adv.Amount / float64(adv.RepaymentMonths)
		adID := adv.ID
		lines = append(lines, lineInput{
			lineType:    entpayl.LineTypeAdvanceRepayment,
			description: fmt.Sprintf("Advance repayment (KES %.2f / %d months)", adv.Amount, adv.RepaymentMonths),
			amount:      -monthlyInstalment,
			advanceID:   &adID,
		})
		totalDeductions += monthlyInstalment
	}

	net := gross - totalDeductions

	payroll, err := h.db.StaffPayroll.Create().
		SetTenantID(tid).
		SetStaffID(staffID).
		SetPeriodStart(periodStart).
		SetPeriodEnd(periodEnd).
		SetGrossAmount(gross).
		SetTotalDeductions(totalDeductions).
		SetNetAmount(net).
		SetStatus("draft").
		Save(r.Context())
	if err != nil {
		h.log.Error("generate payroll failed", zap.Error(err))
		jsonError(w, "failed to generate payroll", http.StatusInternalServerError)
		return
	}

	for _, l := range lines {
		lc := h.db.StaffPayrollLine.Create().
			SetPayrollID(payroll.ID).
			SetLineType(l.lineType).
			SetDescription(l.description).
			SetAmount(l.amount)
		if l.advanceID != nil {
			lc = lc.SetAdvanceID(*l.advanceID)
		}
		if _, err := lc.Save(r.Context()); err != nil {
			h.log.Warn("failed to create payroll line", zap.Error(err))
		}
	}

	payrollLines, _ := h.db.StaffPayrollLine.Query().
		Where(entpayl.PayrollID(payroll.ID)).
		All(r.Context())

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]any{"payroll": payroll, "lines": payrollLines})
}

// GetPayroll handles GET /{tenantID}/pos/payroll/{payrollID}
func (h *PayrollHandler) GetPayroll(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	payrollID, err := uuid.Parse(chi.URLParam(r, "payrollID"))
	if err != nil {
		jsonError(w, "invalid payroll_id", http.StatusBadRequest)
		return
	}

	payroll, err := h.db.StaffPayroll.Query().
		Where(entpay.ID(payrollID), entpay.TenantID(tid)).
		WithLines().
		Only(r.Context())
	if err != nil {
		jsonError(w, "payroll not found", http.StatusNotFound)
		return
	}
	jsonOK(w, payroll)
}

// ApprovePayroll handles POST /{tenantID}/pos/payroll/{payrollID}/approve
func (h *PayrollHandler) ApprovePayroll(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	payrollID, err := uuid.Parse(chi.URLParam(r, "payrollID"))
	if err != nil {
		jsonError(w, "invalid payroll_id", http.StatusBadRequest)
		return
	}

	payroll, err := h.db.StaffPayroll.Query().
		Where(entpay.ID(payrollID), entpay.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "payroll not found", http.StatusNotFound)
		return
	}
	if payroll.Status != "draft" {
		jsonError(w, "only draft payrolls can be approved", http.StatusBadRequest)
		return
	}

	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	approverID, _ := uuid.Parse(claims.Subject)
	now := time.Now()

	updated, err := payroll.Update().
		SetStatus("approved").
		SetApprovedBy(approverID).
		SetApprovedAt(now).
		Save(r.Context())
	if err != nil {
		h.log.Error("approve payroll failed", zap.Error(err))
		jsonError(w, "failed to approve payroll", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// DisbursePayroll handles POST /{tenantID}/pos/payroll/{payrollID}/disburse
// Triggers treasury S2S M-Pesa B2C disbursement for the staff member's net amount.
func (h *PayrollHandler) DisbursePayroll(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	payrollID, err := uuid.Parse(chi.URLParam(r, "payrollID"))
	if err != nil {
		jsonError(w, "invalid payroll_id", http.StatusBadRequest)
		return
	}

	payroll, err := h.db.StaffPayroll.Query().
		Where(entpay.ID(payrollID), entpay.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "payroll not found", http.StatusNotFound)
		return
	}
	if payroll.Status != "approved" {
		jsonError(w, "payroll must be approved before disbursing", http.StatusBadRequest)
		return
	}

	staff, err := h.db.StaffMember.Query().
		Where(staffmember.ID(payroll.StaffID), staffmember.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "staff member not found", http.StatusNotFound)
		return
	}

	tenantSlug := chi.URLParam(r, "tenantID")

	payoutMethod := "mpesa_b2c"
	recipientPhone := ""
	if staff.MpesaPhone != nil && *staff.MpesaPhone != "" {
		recipientPhone = *staff.MpesaPhone
	} else if staff.BankAccountNumber != nil {
		payoutMethod = "paystack_bank"
	}

	if h.treasuryClient != nil {
		resp, disbErr := h.treasuryClient.DisbursePayout(r.Context(), tenantSlug, treasury.PayoutRequest{
			EntityType:   "staff",
			EntityID:     staff.UserID.String(),
			Amount:       payroll.NetAmount,
			Currency:     payroll.Currency,
			Reference:    fmt.Sprintf("payroll-%s", payroll.ID.String()),
			Reason:       fmt.Sprintf("Salary disbursement period %s", payroll.PeriodStart.Format("Jan 2006")),
			PayoutMethod: payoutMethod,
			Recipient: struct {
				Name          string `json:"name"`
				Phone         string `json:"phone,omitempty"`
				AccountNumber string `json:"account_number,omitempty"`
				AccountName   string `json:"account_name,omitempty"`
			}{
				Name:          staff.Name,
				Phone:         recipientPhone,
				AccountNumber: func() string { if staff.BankAccountNumber != nil { return *staff.BankAccountNumber }; return "" }(),
				AccountName:   staff.Name,
			},
		})
		if disbErr != nil {
			h.log.Error("payroll disburse failed", zap.Error(disbErr), zap.Stringer("payroll_id", payroll.ID))
			jsonError(w, "disbursement failed: "+disbErr.Error(), http.StatusBadGateway)
			return
		}
		updated, _ := payroll.Update().
			SetStatus("paid").
			SetPaidAt(time.Now()).
			SetPayoutReference(resp.PayoutID).
			Save(r.Context())
		jsonOK(w, map[string]any{"payroll": updated, "payout": resp})
		return
	}

	// No treasury client — mark as paid with manual reference
	updated, _ := payroll.Update().
		SetStatus("paid").
		SetPaidAt(time.Now()).
		Save(r.Context())
	jsonOK(w, updated)
}
