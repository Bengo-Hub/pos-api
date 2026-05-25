package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/cashdrawer"
	"github.com/bengobox/pos-service/internal/ent/dailyclosing"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posrefund"
)

// DailyClosingHandler handles end-of-day reconciliation endpoints.
type DailyClosingHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewDailyClosingHandler(log *zap.Logger, client *ent.Client) *DailyClosingHandler {
	return &DailyClosingHandler{log: log, client: client}
}

type closeDayInput struct {
	OutletID   string  `json:"outlet_id"`
	CashActual float64 `json:"cash_actual"`
	Notes      string  `json:"notes"`
}

// CloseDayHandler handles POST /{tenantID}/pos/outlets/{outletID}/daily-close
func (h *DailyClosingHandler) CloseDay(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	outletIDStr := chi.URLParam(r, "outletID")
	outletID, err := uuid.Parse(outletIDStr)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}

	var input closeDayInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	now := time.Now().UTC()
	businessDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	// Check if a closing already exists for today.
	existing, err := h.client.DailyClosing.Query().
		Where(
			dailyclosing.TenantID(tid),
			dailyclosing.OutletID(outletID),
			dailyclosing.BusinessDate(businessDate),
		).
		Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		h.log.Error("daily closing query failed", zap.Error(err))
		jsonError(w, "failed to query daily closing", http.StatusInternalServerError)
		return
	}
	if existing != nil && existing.Status == "closed" {
		jsonError(w, "day is already closed", http.StatusConflict)
		return
	}

	// Aggregate today's drawers for this outlet.
	drawers, err := h.client.CashDrawer.Query().
		Where(
			cashdrawer.TenantID(tid),
			cashdrawer.OutletID(outletID),
		).
		All(ctx)
	if err != nil {
		h.log.Error("drawer query failed", zap.Error(err))
		jsonError(w, "failed to aggregate drawers", http.StatusInternalServerError)
		return
	}

	var drawerIDs []uuid.UUID
	var startingCash, cashSales float64
	for _, d := range drawers {
		if d.OpenedAt != nil && d.OpenedAt.UTC().Truncate(24*time.Hour).Equal(businessDate) {
			drawerIDs = append(drawerIDs, d.ID)
			startingCash += d.StartingCash
		}
	}

	// Aggregate today's orders.
	startOfDay := businessDate
	endOfDay := businessDate.Add(24 * time.Hour)

	orders, err := h.client.POSOrder.Query().
		Where(
			posorder.TenantID(tid),
			posorder.OutletID(outletID),
			posorder.StatusEQ("completed"),
			posorder.CreatedAtGTE(startOfDay),
			posorder.CreatedAtLT(endOfDay),
		).
		All(ctx)
	if err != nil {
		h.log.Error("orders query failed", zap.Error(err))
		jsonError(w, "failed to aggregate orders", http.StatusInternalServerError)
		return
	}

	var totalSales, totalDiscounts float64
	for _, o := range orders {
		totalSales += o.TotalAmount
		totalDiscounts += o.DiscountTotal
		cashSales += o.TotalAmount // simplified; ideally filter by cash tender
	}

	// Aggregate today's refunds.
	refunds, err := h.client.POSRefund.Query().
		Where(
			posrefund.OccurredAtGTE(startOfDay),
			posrefund.OccurredAtLT(endOfDay),
		).
		All(ctx)
	if err != nil {
		h.log.Error("refunds query failed", zap.Error(err))
		jsonError(w, "failed to aggregate refunds", http.StatusInternalServerError)
		return
	}
	var totalRefunds float64
	for _, ref := range refunds {
		totalRefunds += ref.Amount
	}

	cashExpected := startingCash + cashSales - totalRefunds
	variance := input.CashActual - cashExpected

	// Get requesting user from claims.
	var closedBy *uuid.UUID
	if userIDStr := r.Header.Get("X-User-ID"); userIDStr != "" {
		if uid, err := uuid.Parse(userIDStr); err == nil {
			closedBy = &uid
		}
	}

	// Upsert the daily closing.
	create := h.client.DailyClosing.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetBusinessDate(businessDate).
		SetTotalSales(totalSales).
		SetTotalRefunds(totalRefunds).
		SetTotalDiscounts(totalDiscounts).
		SetTotalVoids(0).
		SetCashExpected(cashExpected).
		SetCashActual(input.CashActual).
		SetVariance(variance).
		SetStatus("closed").
		SetDrawerIds(drawerIDs)

	if input.Notes != "" {
		create = create.SetNotes(input.Notes)
	}
	if closedBy != nil {
		create = create.SetClosedBy(*closedBy)
	}

	closing, err := create.Save(ctx)
	if err != nil {
		h.log.Error("daily closing create failed", zap.Error(err))
		jsonError(w, "failed to close day", http.StatusInternalServerError)
		return
	}

	jsonOK(w, closing)
}

// ListDailyClosings handles GET /{tenantID}/pos/outlets/{outletID}/daily-closings
func (h *DailyClosingHandler) ListDailyClosings(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	outletIDStr := chi.URLParam(r, "outletID")
	outletID, err := uuid.Parse(outletIDStr)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}

	closings, err := h.client.DailyClosing.Query().
		Where(
			dailyclosing.TenantID(tid),
			dailyclosing.OutletID(outletID),
		).
		Order(ent.Desc(dailyclosing.FieldBusinessDate)).
		Limit(30).
		All(r.Context())
	if err != nil {
		h.log.Error("list daily closings failed", zap.Error(err))
		jsonError(w, "failed to list closings", http.StatusInternalServerError)
		return
	}

	jsonOK(w, closings)
}
