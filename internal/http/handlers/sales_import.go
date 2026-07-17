package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/tender"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// Sales import — POST /{tenantID}/pos/sales/import: bulk import of HISTORICAL sales
// (migration from another POS / spreadsheet records). Lives on POSOrderHandler so it
// shares the orders service without widening the router constructor.
//
// Design notes:
//   - Each row is created through the REAL orders service (orders.Service.CreateOrder),
//     so the stored totals honor the platform identity (total = subtotal + tax − discount
//     + charges + round_off) and order numbers are generated normally.
//   - Idempotency: client_reference = "import:"+external_ref → CreateOrder's get-or-create
//     short-circuit makes re-uploading the same file safe (rows report "skipped").
//   - The row's sale date lands on business_date (the same admin reporting-date override
//     Move Sale Date uses), so historical sales report on the day they actually happened.
//   - A payment_method marks the sale settled: a completed POSPayment on the tenant's
//     matching tender (when one exists) + paid_total = total. No treasury intent is
//     created and pos.sale.finalized is NOT published — imported history must not
//     re-deduct stock, re-earn loyalty, or re-post GL for sales an old system already
//     accounted for.

type importSaleLine struct {
	CatalogItemID string  `json:"catalog_item_id,omitempty"` // resolved by the UI from the catalog; optional
	SKU           string  `json:"sku"`
	Name          string  `json:"name"`
	Quantity      float64 `json:"quantity"`
	UnitPrice     float64 `json:"unit_price"`
}

type importSaleRow struct {
	ExternalRef   string           `json:"external_ref"` // required — idempotency key (old system's invoice no.)
	Date          string           `json:"date,omitempty"`
	CustomerName  string           `json:"customer_name,omitempty"`
	CustomerPhone string           `json:"customer_phone,omitempty"`
	Discount      float64          `json:"discount,omitempty"`
	PaymentMethod string           `json:"payment_method,omitempty"` // cash|mpesa|card|... empty = imported as due
	Note          string           `json:"note,omitempty"`
	Lines         []importSaleLine `json:"lines"`
}

type importSalesRequest struct {
	OutletID string          `json:"outlet_id"`
	Rows     []importSaleRow `json:"rows"`
}

type importSaleRowResult struct {
	ExternalRef string `json:"external_ref"`
	OrderNumber string `json:"order_number,omitempty"`
	Status      string `json:"status"` // imported | skipped | failed
	Error       string `json:"error,omitempty"`
}

type importSalesResult struct {
	Imported int                   `json:"imported"`
	Skipped  int                   `json:"skipped"`
	Failed   int                   `json:"failed"`
	Results  []importSaleRowResult `json:"results"`
}

// parseImportDate accepts YYYY-MM-DD or RFC3339.
func parseImportDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// ImportSales handles POST /{tenantID}/pos/sales/import (pos.orders.manage).
func (h *POSOrderHandler) ImportSales(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var req importSalesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(req.Rows) == 0 {
		jsonError(w, "rows array is required and must be non-empty", http.StatusBadRequest)
		return
	}
	if len(req.Rows) > 500 {
		jsonError(w, "maximum 500 rows per import request — split the file", http.StatusBadRequest)
		return
	}

	outletID, err := uuid.Parse(req.OutletID)
	if err != nil {
		// Fall back to the request's outlet context (X-Outlet-ID) when no explicit outlet was sent.
		if oid := httpware.GetOutletID(r.Context()); oid != "" {
			outletID, err = uuid.Parse(oid)
		}
		if err != nil {
			jsonError(w, "outlet_id required", http.StatusBadRequest)
			return
		}
	}

	tenantSlug := ""
	userID := uuid.Nil
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		tenantSlug = claims.GetTenantSlug()
		if claims.Subject != "" {
			userID, _ = uuid.Parse(claims.Subject)
		}
	}

	// Resolve the tenant's tenders ONCE (type → id) for payment stamping.
	tenderByType := map[string]uuid.UUID{}
	if tenders, terr := h.client.Tender.Query().Where(tender.TenantID(tid), tender.IsActive(true)).All(r.Context()); terr == nil {
		for _, t := range tenders {
			tenderByType[strings.ToLower(t.Type)] = t.ID
		}
	}

	result := importSalesResult{Results: make([]importSaleRowResult, 0, len(req.Rows))}
	for _, row := range req.Rows {
		rr := h.importRow(r.Context(), tid, tenantSlug, outletID, userID, tenderByType, row)
		switch rr.Status {
		case "imported":
			result.Imported++
		case "skipped":
			result.Skipped++
		default:
			result.Failed++
			h.log.Warn("sales import: row failed", zap.String("external_ref", row.ExternalRef), zap.String("error", rr.Error))
		}
		result.Results = append(result.Results, rr)
	}

	jsonOK(w, result)
}

func (h *POSOrderHandler) importRow(
	ctx context.Context,
	tid uuid.UUID,
	tenantSlug string,
	outletID, userID uuid.UUID,
	tenderByType map[string]uuid.UUID,
	row importSaleRow,
) importSaleRowResult {
	rr := importSaleRowResult{ExternalRef: row.ExternalRef, Status: "failed"}
	if strings.TrimSpace(row.ExternalRef) == "" {
		rr.Error = "external_ref is required (the old system's invoice number)"
		return rr
	}
	if len(row.Lines) == 0 {
		rr.Error = "at least one line is required"
		return rr
	}

	clientRef := "import:" + strings.TrimSpace(row.ExternalRef)

	// Replay detection BEFORE creating: CreateOrder get-or-creates on client_reference,
	// so an existing order means this row was already imported.
	if existing, err := h.client.POSOrder.Query().
		Where(posorder.TenantID(tid), posorder.ClientReference(clientRef)).
		Only(ctx); err == nil && existing != nil {
		rr.Status = "skipped"
		rr.OrderNumber = existing.OrderNumber
		return rr
	}

	lines := make([]orders.OrderLineInput, 0, len(row.Lines))
	for _, l := range row.Lines {
		if l.Quantity <= 0 {
			rr.Error = fmt.Sprintf("line %q: quantity must be positive", l.SKU)
			return rr
		}
		itemID := uuid.Nil
		if l.CatalogItemID != "" {
			itemID, _ = uuid.Parse(l.CatalogItemID)
		}
		if itemID == uuid.Nil {
			// No catalog match — a deterministic UUID from the SKU keeps the line storable
			// and the same SKU maps to the same id on every import run.
			itemID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("pos-import-sku:"+l.SKU))
		}
		name := l.Name
		if name == "" {
			name = l.SKU
		}
		lines = append(lines, orders.OrderLineInput{
			CatalogItemID: itemID,
			SKU:           l.SKU,
			Name:          name,
			Quantity:      l.Quantity,
			UnitPrice:     l.UnitPrice,
			TotalPrice:    l.UnitPrice * l.Quantity,
			Metadata:      map[string]any{"imported": true},
		})
	}

	meta := map[string]any{
		"imported":   true,
		"import_ref": row.ExternalRef,
	}
	if row.Note != "" {
		meta["staff_note"] = row.Note
	}
	if row.PaymentMethod != "" {
		meta["payment_method"] = strings.ToLower(strings.TrimSpace(row.PaymentMethod))
	}

	order, err := h.orderSvc.CreateOrder(ctx, orders.CreateOrderRequest{
		TenantID:        tid,
		TenantSlug:      tenantSlug,
		OutletID:        outletID,
		UserID:          userID,
		ClientReference: clientRef,
		Currency:        "KES",
		Lines:           lines,
		Metadata:        meta,
		OrderSubtype:    "retail",
		CustomerPhone:   row.CustomerPhone,
		CustomerName:    row.CustomerName,
		DiscountAmount:  row.Discount,
		Source:          "import",
	})
	if err != nil {
		rr.Error = err.Error()
		return rr
	}

	// Historical completion — direct row updates, deliberately NOT the payment/completion
	// pipeline (no treasury intent, no pos.sale.finalized → no stock/loyalty/GL side effects).
	upd := h.client.POSOrder.UpdateOneID(order.ID).SetStatus("completed")
	if saleDate, ok := parseImportDate(row.Date); ok {
		upd = upd.SetBusinessDate(saleDate)
	}
	method := strings.ToLower(strings.TrimSpace(row.PaymentMethod))
	if method != "" {
		upd = upd.SetPaidTotal(order.TotalAmount)
	}
	if _, err := upd.Save(ctx); err != nil {
		rr.Error = "order created but completion failed: " + err.Error()
		rr.OrderNumber = order.OrderNumber
		return rr
	}
	if method != "" {
		if tenderID, ok := tenderByType[method]; ok {
			occurred := time.Now()
			if saleDate, ok := parseImportDate(row.Date); ok {
				occurred = saleDate
			}
			if _, perr := h.client.POSPayment.Create().
				SetOrderID(order.ID).
				SetTenderID(tenderID).
				SetAmount(order.TotalAmount).
				SetStatus("completed").
				SetExternalReference("import:" + row.ExternalRef).
				SetPaymentData(map[string]any{"method": method, "source": "import"}).
				SetOccurredAt(occurred).
				Save(ctx); perr != nil {
				h.log.Warn("sales import: payment row failed (order still marked paid)",
					zap.String("order", order.OrderNumber), zap.Error(perr))
			}
		}
	}

	rr.Status = "imported"
	rr.OrderNumber = order.OrderNumber
	return rr
}
