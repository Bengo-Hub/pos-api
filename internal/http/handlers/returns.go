package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/modules/returns"
)

// ReturnHandler is a thin HTTP adapter over returns.Service — it decodes requests, resolves
// tenant/user context off the *http.Request, and shapes JSON responses (order-number
// enrichment for the UI). All business logic (guards, settlement, exchange fulfilment) lives
// in returns.Service so other modules (saleedit's fiscalized-reduction path) can reuse it.
type ReturnHandler struct {
	log    *zap.Logger
	client *ent.Client
	svc    *returns.Service
	// cache/authURL back the printed refund receipt (see returns_receipt.go): tenant branding
	// (logo/company name/contact fallbacks) and the platform-owner footer are both resolved
	// through the shared tenant cache, exactly as ReceiptHandler does for a sale receipt.
	cache   *sharedcache.Aside
	authURL string
}

func NewReturnHandler(log *zap.Logger, client *ent.Client, svc *returns.Service) *ReturnHandler {
	return &ReturnHandler{log: log, client: client, svc: svc}
}

// WithBranding wires the shared tenant cache + auth-api URL used by the refund receipt endpoint.
// Optional; nil/empty simply prints with the local tenant name and the static platform-owner
// footer (never blocks printing).
func (h *ReturnHandler) WithBranding(cache *sharedcache.Aside, authURL string) *ReturnHandler {
	h.cache = cache
	h.authURL = authURL
	return h
}

func requestUserID(r *http.Request) uuid.UUID {
	if userIDStr := r.Header.Get("X-User-ID"); userIDStr != "" {
		if uid, err := uuid.Parse(userIDStr); err == nil {
			return uid
		}
	}
	return uuid.Nil
}

type returnLineInput struct {
	OrderLineID      uuid.UUID `json:"order_line_id"`
	CatalogItemID    uuid.UUID `json:"catalog_item_id,omitempty"`
	SKU              string    `json:"sku"`
	Name             string    `json:"name"`
	Quantity         float64   `json:"quantity"`
	UnitPrice        float64   `json:"unit_price"`
	TotalPrice       float64   `json:"total_price"`
	Reason           string    `json:"reason"`
	TaxCodeID        string    `json:"tax_code_id,omitempty"`
	PriceIncludesTax bool      `json:"price_includes_tax,omitempty"`
	TaxRate          *float64  `json:"tax_rate,omitempty"`
}

func toLineInputs(in []returnLineInput) []returns.LineInput {
	out := make([]returns.LineInput, 0, len(in))
	for _, l := range in {
		out = append(out, returns.LineInput{
			OrderLineID: l.OrderLineID, CatalogItemID: l.CatalogItemID, SKU: l.SKU, Name: l.Name,
			Quantity: l.Quantity, UnitPrice: l.UnitPrice, TotalPrice: l.TotalPrice, Reason: l.Reason,
			TaxCodeID: l.TaxCodeID, PriceIncludesTax: l.PriceIncludesTax, TaxRate: l.TaxRate,
		})
	}
	return out
}

type createReturnInput struct {
	OutletID      string            `json:"outlet_id"`
	ReturnType    string            `json:"return_type"`
	Reason        string            `json:"reason"`
	ReasonCode    string            `json:"reason_code,omitempty"`
	RefundChannel string            `json:"refund_channel,omitempty"`
	// ReturnDate optionally backdates the return; accepts "YYYY-MM-DD" or RFC3339 (see
	// parseFlexibleDate in hotel.go). Omitted = now, same as before this field existed.
	ReturnDate string            `json:"return_date,omitempty"`
	Lines      []returnLineInput `json:"lines"`
}

type approveReturnInput struct {
	Action        string `json:"action"`
	Notes         string `json:"notes"`
	RefundChannel string `json:"refund_channel,omitempty"`
}

type completeReturnInput struct {
	Notes         string            `json:"notes,omitempty"`
	RefundChannel string            `json:"refund_channel,omitempty"`
	ExchangeLines []returnLineInput `json:"exchange_lines,omitempty"`
}

// returnResponse decorates a POSReturn with the original order's human-readable number so the UI
// never has to render the raw order UUID ("Original Order"). Embedding the *ent.POSReturn promotes
// all of its JSON fields (incl. the `edges.lines`), and adds `order_number` alongside them.
type returnResponse struct {
	*ent.POSReturn
	OrderNumber   string `json:"order_number,omitempty"`
	CustomerName  string `json:"customer_name,omitempty"`
	CustomerPhone string `json:"customer_phone,omitempty"`
}

func (h *ReturnHandler) orderNumberFor(ctx context.Context, tid, orderID uuid.UUID) string {
	if orderID == uuid.Nil {
		return ""
	}
	o, err := h.client.POSOrder.Query().
		Where(entposorder.ID(orderID), entposorder.TenantID(tid)).
		Select(entposorder.FieldOrderNumber).
		Only(ctx)
	if err != nil || o == nil {
		return ""
	}
	return o.OrderNumber
}

func (h *ReturnHandler) withOrderNumber(ctx context.Context, tid uuid.UUID, ret *ent.POSReturn) returnResponse {
	resp := returnResponse{POSReturn: ret}
	if ret.OrderID == uuid.Nil {
		return resp
	}
	o, err := h.client.POSOrder.Query().
		Where(entposorder.ID(ret.OrderID), entposorder.TenantID(tid)).
		Select(entposorder.FieldOrderNumber, entposorder.FieldCustomerName, entposorder.FieldCustomerPhone).
		Only(ctx)
	if err != nil || o == nil {
		return resp
	}
	resp.OrderNumber = o.OrderNumber
	if o.CustomerName != nil {
		resp.CustomerName = *o.CustomerName
	}
	if o.CustomerPhone != nil {
		resp.CustomerPhone = *o.CustomerPhone
	}
	return resp
}

func (h *ReturnHandler) withOrderNumbers(ctx context.Context, tid uuid.UUID, rets []*ent.POSReturn) []returnResponse {
	ids := make([]uuid.UUID, 0, len(rets))
	for _, ret := range rets {
		if ret.OrderID != uuid.Nil {
			ids = append(ids, ret.OrderID)
		}
	}
	type orderInfo struct {
		number, custName, custPhone string
	}
	infoByID := make(map[uuid.UUID]orderInfo, len(ids))
	if len(ids) > 0 {
		ords, err := h.client.POSOrder.Query().
			Where(entposorder.TenantID(tid), entposorder.IDIn(ids...)).
			Select(entposorder.FieldID, entposorder.FieldOrderNumber, entposorder.FieldCustomerName, entposorder.FieldCustomerPhone).
			All(ctx)
		if err == nil {
			for _, o := range ords {
				info := orderInfo{number: o.OrderNumber}
				if o.CustomerName != nil {
					info.custName = *o.CustomerName
				}
				if o.CustomerPhone != nil {
					info.custPhone = *o.CustomerPhone
				}
				infoByID[o.ID] = info
			}
		}
	}
	out := make([]returnResponse, 0, len(rets))
	for _, ret := range rets {
		info := infoByID[ret.OrderID]
		out = append(out, returnResponse{POSReturn: ret, OrderNumber: info.number, CustomerName: info.custName, CustomerPhone: info.custPhone})
	}
	return out
}

// CreateReturn handles POST /{tenantID}/pos/orders/{orderID}/returns
func (h *ReturnHandler) CreateReturn(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}
	var input createReturnInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	requestedBy := requestUserID(r)
	var returnDate *time.Time
	if input.ReturnDate != "" {
		if t, derr := parseFlexibleDate(input.ReturnDate); derr == nil {
			returnDate = &t
		}
	}
	ret, err := h.svc.CreateReturn(r.Context(), tid, returns.CreateReturnRequest{
		OrderID: orderID, OutletID: parseOptionalUUID(input.OutletID, r), ReturnType: input.ReturnType,
		Reason: input.Reason, ReasonCode: input.ReasonCode, RefundChannel: input.RefundChannel,
		Lines: toLineInputs(input.Lines), RequestedBy: requestedBy, ReturnDate: returnDate,
	})
	if err != nil {
		status := classifyReturnError(err)
		jsonError(w, err.Error(), status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(returnResponse{POSReturn: ret, OrderNumber: h.orderNumberFor(r.Context(), tid, orderID)})
}

// ListReturns handles GET /{tenantID}/pos/returns
func (h *ReturnHandler) ListReturns(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	p := pagination.Parse(r)
	baseQ := h.client.POSReturn.Query().Where(posreturn.TenantID(tid))

	urlq := r.URL.Query()
	if status := urlq.Get("status"); status != "" {
		baseQ = baseQ.Where(posreturn.StatusEQ(posreturn.Status(status)))
	}
	if staffIDStr := urlq.Get("staff_id"); staffIDStr != "" {
		if staffUID, err := uuid.Parse(staffIDStr); err == nil {
			baseQ = baseQ.Where(posreturn.RequestedBy(staffUID))
		}
	}
	if orderIDStr := urlq.Get("order_id"); orderIDStr != "" {
		if orderUID, err := uuid.Parse(orderIDStr); err == nil {
			baseQ = baseQ.Where(posreturn.OrderID(orderUID))
		}
	}
	if from, to, ok := parseCreatedAtRange(r); ok {
		baseQ = baseQ.Where(posreturn.CreatedAtGTE(from), posreturn.CreatedAtLTE(to))
	}

	total, _ := baseQ.Clone().Count(r.Context())
	rets, err := baseQ.WithLines().Order(ent.Desc(posreturn.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list returns failed", zap.Error(err))
		jsonError(w, "failed to list returns", http.StatusInternalServerError)
		return
	}

	jsonOK(w, pagination.NewResponse(h.withOrderNumbers(r.Context(), tid, rets), total, p))
}

// GetReturn handles GET /{tenantID}/pos/returns/{returnID}
func (h *ReturnHandler) GetReturn(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	returnID, err := uuid.Parse(chi.URLParam(r, "returnID"))
	if err != nil {
		jsonError(w, "invalid return_id", http.StatusBadRequest)
		return
	}

	ret, err := h.client.POSReturn.Query().
		Where(posreturn.ID(returnID), posreturn.TenantID(tid)).
		WithLines().
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "return not found", http.StatusNotFound)
			return
		}
		jsonError(w, "failed to get return", http.StatusInternalServerError)
		return
	}

	jsonOK(w, h.withOrderNumber(r.Context(), tid, ret))
}

// ApproveReturn handles PATCH /{tenantID}/pos/returns/{returnID}/approve
func (h *ReturnHandler) ApproveReturn(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	returnID, err := uuid.Parse(chi.URLParam(r, "returnID"))
	if err != nil {
		jsonError(w, "invalid return_id", http.StatusBadRequest)
		return
	}
	var input approveReturnInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	updated, err := h.svc.ApproveReturn(r.Context(), tid, returnID, returns.ApproveReturnRequest{
		Action: input.Action, Notes: input.Notes, RefundChannel: input.RefundChannel, ApproverID: requestUserID(r),
	})
	if err != nil {
		jsonError(w, err.Error(), classifyReturnError(err))
		return
	}

	jsonOK(w, h.withOrderNumber(r.Context(), tid, updated))
}

// CompleteReturn handles POST /{tenantID}/pos/returns/{returnID}/complete — the final fulfilment
// step. Only an APPROVED return can be completed; it settles the money (treasury refund + eTIMS
// credit note) and publishes return.completed/exchange.completed (inventory restock + treasury
// settlement), then marks the return completed so it lands in the Completed tab.
func (h *ReturnHandler) CompleteReturn(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	returnID, err := uuid.Parse(chi.URLParam(r, "returnID"))
	if err != nil {
		jsonError(w, "invalid return_id", http.StatusBadRequest)
		return
	}

	var input completeReturnInput
	_ = json.NewDecoder(r.Body).Decode(&input) // optional body

	tenantSlug := chi.URLParam(r, "tenantID")
	updated, exchange, err := h.svc.CompleteReturn(r.Context(), tid, tenantSlug, returnID, returns.CompleteReturnRequest{
		Notes: input.Notes, RefundChannel: input.RefundChannel, ExchangeLines: toLineInputs(input.ExchangeLines),
		CompletedBy: requestUserID(r),
	})
	if err != nil {
		jsonError(w, err.Error(), classifyReturnError(err))
		return
	}

	jsonOK(w, struct {
		returnResponse
		Exchange *returns.ExchangeResult `json:"exchange,omitempty"`
	}{h.withOrderNumber(r.Context(), tid, updated), exchange})
}

// classifyReturnError maps a returns.Service error message to the HTTP status the old
// inline handlers used to return for the equivalent condition. The service layer returns
// plain errors (no status codes — it has no HTTP dependency), so the adapter classifies by
// message here, at the one place that still needs an http.Status.
func classifyReturnError(err error) int {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "load order:"), strings.HasPrefix(msg, "create return:"),
		strings.HasPrefix(msg, "update return:"), strings.HasPrefix(msg, "complete return:"),
		strings.HasPrefix(msg, "get return:"):
		return http.StatusInternalServerError
	case strings.Contains(msg, "not found"):
		return http.StatusNotFound
	case strings.Contains(msg, "return is not pending"), strings.Contains(msg, "only an approved return can be completed"):
		return http.StatusConflict
	case strings.Contains(msg, "at least one return line is required"):
		return http.StatusBadRequest
	default:
		// Every other business-rule rejection (return window expired, non-returnable item,
		// refund-channel policy violation, exchange fulfilment failure) is a 422 in the
		// original per-endpoint handlers.
		return http.StatusUnprocessableEntity
	}
}
