package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entbillsplit "github.com/bengobox/pos-service/internal/ent/billsplit"
)

// BillSplitHandler manages bill splitting for POS orders.
type BillSplitHandler struct {
	log    *zap.Logger
	client *ent.Client
}

func NewBillSplitHandler(log *zap.Logger, client *ent.Client) *BillSplitHandler {
	return &BillSplitHandler{log: log, client: client}
}

// ListSplits handles GET /{tenantID}/pos/orders/{orderID}/splits
func (h *BillSplitHandler) ListSplits(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order id", http.StatusBadRequest)
		return
	}

	p := pagination.Parse(r)
	baseQ := h.client.BillSplit.Query().Where(entbillsplit.TenantID(tid), entbillsplit.OrderID(orderID))
	count, _ := baseQ.Clone().Count(r.Context())
	splits, err := baseQ.Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list splits failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	var totalAmount float64
	paid := 0.0
	for _, s := range splits {
		totalAmount += s.Amount
		if s.Status == "paid" {
			paid += s.Amount
		}
	}

	resp := pagination.NewResponse(splits, count, p)
	jsonOK(w, map[string]any{
		"data":         resp,
		"total_amount": totalAmount,
		"paid_amount":  paid,
		"balance":      totalAmount - paid,
	})
}

type createSplitInput struct {
	Splits []struct {
		Label  string  `json:"label"`
		Amount float64 `json:"amount"`
	} `json:"splits"`
}

// CreateSplits handles POST /{tenantID}/pos/orders/{orderID}/splits
// Replaces any existing splits for the order.
func (h *BillSplitHandler) CreateSplits(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order id", http.StatusBadRequest)
		return
	}

	var input createSplitInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(input.Splits) == 0 {
		jsonError(w, "at least one split is required", http.StatusBadRequest)
		return
	}

	// Delete any existing pending splits for this order
	_, _ = h.client.BillSplit.Delete().
		Where(entbillsplit.TenantID(tid), entbillsplit.OrderID(orderID), entbillsplit.StatusEQ("pending")).
		Exec(r.Context())

	creates := make([]*ent.BillSplitCreate, 0, len(input.Splits))
	for _, s := range input.Splits {
		creates = append(creates, h.client.BillSplit.Create().
			SetTenantID(tid).
			SetOrderID(orderID).
			SetSplitLabel(s.Label).
			SetAmount(s.Amount))
	}

	splits, err := h.client.BillSplit.CreateBulk(creates...).Save(r.Context())
	if err != nil {
		h.log.Error("create splits failed", zap.Error(err))
		jsonError(w, "failed to create splits", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]any{"data": splits, "total": len(splits)})
}

type settleSplitInput struct {
	PaymentMethod string `json:"payment_method"`
	ExternalRef   string `json:"external_ref,omitempty"`
}

// SettleSplit handles POST /{tenantID}/pos/orders/{orderID}/splits/{splitID}/settle
func (h *BillSplitHandler) SettleSplit(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	splitID, err := uuid.Parse(chi.URLParam(r, "splitID"))
	if err != nil {
		jsonError(w, "invalid split id", http.StatusBadRequest)
		return
	}

	var input settleSplitInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	split, err := h.client.BillSplit.Query().
		Where(entbillsplit.ID(splitID), entbillsplit.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "split not found", http.StatusNotFound)
		return
	}
	if split.Status == "paid" {
		jsonError(w, "split already paid", http.StatusConflict)
		return
	}

	upd := split.Update().
		SetStatus("paid").
		SetPaymentMethod(input.PaymentMethod)
	if input.ExternalRef != "" {
		upd = upd.SetExternalRef(input.ExternalRef)
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("settle split failed", zap.Error(err))
		jsonError(w, "failed to settle split", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}
