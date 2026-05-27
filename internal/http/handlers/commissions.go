package handlers

import (
	"net/http"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entcr "github.com/bengobox/pos-service/internal/ent/commissionrecord"
)

type CommissionHandler struct {
	log *zap.Logger
	db  *ent.Client
}

func NewCommissionHandler(log *zap.Logger, db *ent.Client) *CommissionHandler {
	return &CommissionHandler{log: log, db: db}
}

// List handles GET /{tenantID}/pos/commissions
// Query params: staff_member_id, order_id
func (h *CommissionHandler) List(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.CommissionRecord.Query().Where(entcr.TenantID(tid))

	if smID := r.URL.Query().Get("staff_member_id"); smID != "" {
		staffID, err := uuid.Parse(smID)
		if err == nil {
			q = q.Where(entcr.StaffMemberID(staffID))
		}
	}
	if oID := r.URL.Query().Get("order_id"); oID != "" {
		orderID, err := uuid.Parse(oID)
		if err == nil {
			q = q.Where(entcr.OrderID(orderID))
		}
	}

	p := pagination.Parse(r)
	total, _ := q.Clone().Count(r.Context())
	records, err := q.Order(ent.Desc(entcr.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list commissions failed", zap.Error(err))
		jsonError(w, "failed to list commissions", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(records, total, p))
}

// Get handles GET /{tenantID}/pos/commissions/{id}
func (h *CommissionHandler) Get(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	crID, err := uuid.Parse(chi.URLParam(r, "commissionID"))
	if err != nil {
		jsonError(w, "invalid commission_id", http.StatusBadRequest)
		return
	}

	record, err := h.db.CommissionRecord.Get(r.Context(), crID)
	if err != nil || record.TenantID != tid {
		jsonError(w, "commission record not found", http.StatusNotFound)
		return
	}
	jsonOK(w, record)
}
