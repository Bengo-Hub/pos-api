package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	entposreversal "github.com/bengobox/pos-service/internal/ent/posreversal"
	"github.com/bengobox/pos-service/internal/modules/reversals"
)

// ReversalHandler exposes the platform-owner transaction-reversal tool: reverse a
// finalized sale (whole order or items) across pos/inventory/treasury/eTIMS with a
// tracked per-step ledger. Routes are gated to platform owners in the router.
type ReversalHandler struct {
	log *zap.Logger
	svc *reversals.Service
}

// NewReversalHandler constructs the handler.
func NewReversalHandler(log *zap.Logger, svc *reversals.Service) *ReversalHandler {
	return &ReversalHandler{log: log, svc: svc}
}

type createReversalInput struct {
	OrderID        uuid.UUID                 `json:"order_id"`
	Scope          string                    `json:"scope"` // full | partial
	Lines          []reversals.LineSelection `json:"lines,omitempty"`
	Reason         string                    `json:"reason"`
	RefundChannel  string                    `json:"refund_channel,omitempty"`
	IdempotencyKey string                    `json:"idempotency_key,omitempty"`
}

// CreateReversal handles POST /{tenantID}/pos/reversals — executes the reversal
// synchronously and returns the record with its per-step outcome.
func (h *ReversalHandler) CreateReversal(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var input createReversalInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	requestedBy, _ := uuid.Parse(claims.Subject)

	rev, err := h.svc.Execute(r.Context(), tid, reversals.CreateRequest{
		OrderID:        input.OrderID,
		Scope:          input.Scope,
		Lines:          input.Lines,
		Reason:         input.Reason,
		RefundChannel:  input.RefundChannel,
		IdempotencyKey: input.IdempotencyKey,
		TenantSlug:     chi.URLParam(r, "tenantID"),
		RequestedBy:    requestedBy,
	})
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(rev)
}

// ListReversals handles GET /{tenantID}/pos/reversals — the sync-monitor tab's history
// list. Filters: status, order (number contains), plus standard pagination.
func (h *ReversalHandler) ListReversals(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	p := pagination.Parse(r)
	q := h.svc.Client().POSReversal.Query().Where(entposreversal.TenantID(tid))
	if status := r.URL.Query().Get("status"); status != "" {
		q = q.Where(entposreversal.StatusEQ(entposreversal.Status(status)))
	}
	if search := r.URL.Query().Get("order"); search != "" {
		q = q.Where(entposreversal.OrderNumberContainsFold(search))
	}
	total, _ := q.Clone().Count(r.Context())
	rows, err := q.Order(ent.Desc(entposreversal.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list reversals failed", zap.Error(err))
		jsonError(w, "failed to list reversals", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(rows, total, p))
}

// GetReversal handles GET /{tenantID}/pos/reversals/{reversalID}.
func (h *ReversalHandler) GetReversal(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	revID, err := uuid.Parse(chi.URLParam(r, "reversalID"))
	if err != nil {
		jsonError(w, "invalid reversal id", http.StatusBadRequest)
		return
	}
	rev, err := h.svc.Client().POSReversal.Query().
		Where(entposreversal.ID(revID), entposreversal.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "reversal not found", http.StatusNotFound)
		return
	}
	jsonOK(w, rev)
}

// RetryReversal handles POST /{tenantID}/pos/reversals/{reversalID}/retry — re-runs the
// failed/pending steps (each downstream call is idempotent).
func (h *ReversalHandler) RetryReversal(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	revID, err := uuid.Parse(chi.URLParam(r, "reversalID"))
	if err != nil {
		jsonError(w, "invalid reversal id", http.StatusBadRequest)
		return
	}
	rev, err := h.svc.Retry(r.Context(), tid, revID, chi.URLParam(r, "tenantID"))
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	jsonOK(w, rev)
}
