package handlers

import (
	"net/http"
	"sort"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/posrefund"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/ent/tender"
)

// registerDetailsResponse is the detailed register/period report powering the POS
// "Register Details" modal — a payment-method breakdown, sales/refund/payment/credit
// totals, the list of products sold, and products grouped by brand.
type registerDetailsResponse struct {
	From         time.Time             `json:"from"`
	To           time.Time             `json:"to"`
	PaymentMethods []paymentMethodRow  `json:"payment_methods"`
	TotalSales   float64               `json:"total_sales"`
	TotalRefund  float64               `json:"total_refund"`
	RefundByMethod []paymentMethodRow  `json:"refund_by_method"`
	TotalPayment float64               `json:"total_payment"`
	CreditSales  float64               `json:"credit_sales"`
	TotalExpense float64               `json:"total_expense"`
	OrderTax     float64               `json:"order_tax"`
	ShippingTotal float64              `json:"shipping_total"`
	GrandTotal   float64               `json:"grand_total"`
	OrderCount   int                   `json:"order_count"`
	RefundCount  int                   `json:"refund_count"`
	ProductsSold []productSoldRow      `json:"products_sold"`
	ProductsByBrand []brandSoldRow     `json:"products_by_brand"`
}

type paymentMethodRow struct {
	Method      string  `json:"method"`       // canonical tender type (cash, cheque, card, bank_transfer, ...)
	SellAmount  float64 `json:"sell_amount"`
	ExpenseAmount float64 `json:"expense_amount"`
}

type productSoldRow struct {
	SKU         string  `json:"sku"`
	Name        string  `json:"name"`
	Quantity    float64 `json:"quantity"`
	TotalAmount float64 `json:"total_amount"`
}

type brandSoldRow struct {
	Brand       string  `json:"brand"`
	Quantity    float64 `json:"quantity"`
	TotalAmount float64 `json:"total_amount"`
}

// RegisterDetails handles GET /{tenantID}/pos/reports/register-details?from=&to=&outlet_id=
// It aggregates completed sales in the window into the GoDigital-style register report.
func (h *ReportsHandler) RegisterDetails(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	from, to := parseDateRange(r)

	// Outlet scope: explicit ?outlet_id wins; else the header outlet context.
	filters := []predicate.POSOrder{posorder.TenantID(tid), posorder.CreatedAtGTE(from), posorder.CreatedAtLTE(to)}
	if outletParam := r.URL.Query().Get("outlet_id"); outletParam != "" && outletParam != "all" {
		if oid, perr := uuid.Parse(outletParam); perr == nil {
			filters = append(filters, posorder.OutletID(oid))
		}
	} else if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, perr := uuid.Parse(oidStr); perr == nil {
			filters = append(filters, posorder.OutletID(oid))
		}
	}

	orders, err := h.db.POSOrder.Query().Where(filters...).WithLines().WithPayments().All(r.Context())
	if err != nil {
		h.log.Error("register-details orders query failed", zap.Error(err))
		jsonError(w, "failed to build register details", http.StatusInternalServerError)
		return
	}

	// Resolve tender types once for every payment in the window.
	tenderType := map[uuid.UUID]string{}
	{
		idSet := map[uuid.UUID]struct{}{}
		for _, o := range orders {
			for _, p := range o.Edges.Payments {
				idSet[p.TenderID] = struct{}{}
			}
		}
		if len(idSet) > 0 {
			ids := make([]uuid.UUID, 0, len(idSet))
			for id := range idSet {
				ids = append(ids, id)
			}
			if tenders, terr := h.db.Tender.Query().Where(tender.IDIn(ids...)).All(r.Context()); terr == nil {
				for _, t := range tenders {
					tenderType[t.ID] = t.Type
				}
			}
		}
	}

	resp := registerDetailsResponse{From: from, To: to}
	methodSell := map[string]float64{}
	productAgg := map[string]*productSoldRow{}
	orderIDs := make([]uuid.UUID, 0, len(orders))

	for _, o := range orders {
		orderIDs = append(orderIDs, o.ID)
		if o.Status != "completed" {
			continue
		}
		resp.OrderCount++
		resp.TotalSales += o.TotalAmount
		resp.OrderTax += o.TaxTotal
		if sc, ok := o.Metadata["shipping_amount"].(float64); ok {
			resp.ShippingTotal += sc
		}

		paid := 0.0
		for _, p := range o.Edges.Payments {
			if p.Status != "completed" {
				continue
			}
			paid += p.Amount
			method := tenderType[p.TenderID]
			if method == "" {
				method = "other"
			}
			methodSell[method] += p.Amount
			resp.TotalPayment += p.Amount
		}
		if due := o.TotalAmount - paid; due > 0.009 {
			resp.CreditSales += due
		}

		for _, l := range o.Edges.Lines {
			key := l.Sku
			if key == "" {
				key = l.Name
			}
			row, ok := productAgg[key]
			if !ok {
				row = &productSoldRow{SKU: l.Sku, Name: l.Name}
				productAgg[key] = row
			}
			row.Quantity += l.Quantity
			row.TotalAmount += l.TotalPrice
		}
	}

	// Refunds that occurred in the window on this period's orders, split by original tender.
	refundByMethod := map[string]float64{}
	if len(orderIDs) > 0 {
		refunds, rerr := h.db.POSRefund.Query().
			Where(
				posrefund.OrderIDIn(orderIDs...),
				posrefund.StatusEQ("completed"),
				posrefund.OccurredAtGTE(from),
				posrefund.OccurredAtLTE(to),
			).All(r.Context())
		if rerr == nil {
			for _, rf := range refunds {
				resp.TotalRefund += rf.Amount
				resp.RefundCount++
				method := "cash"
				if rf.PaymentID != nil {
					if pay, perr := h.db.POSPayment.Query().Where(pospayment.ID(*rf.PaymentID)).Only(r.Context()); perr == nil {
						if mt := tenderType[pay.TenderID]; mt != "" {
							method = mt
						}
					}
				}
				refundByMethod[method] += rf.Amount
			}
		}
	}

	// Payment-method rows sorted by amount desc.
	for method, amt := range methodSell {
		resp.PaymentMethods = append(resp.PaymentMethods, paymentMethodRow{Method: method, SellAmount: amt})
	}
	sort.Slice(resp.PaymentMethods, func(i, j int) bool {
		return resp.PaymentMethods[i].SellAmount > resp.PaymentMethods[j].SellAmount
	})
	for method, amt := range refundByMethod {
		resp.RefundByMethod = append(resp.RefundByMethod, paymentMethodRow{Method: method, SellAmount: amt})
	}
	sort.Slice(resp.RefundByMethod, func(i, j int) bool {
		return resp.RefundByMethod[i].SellAmount > resp.RefundByMethod[j].SellAmount
	})

	// Products sold, sorted by revenue desc.
	skus := make([]string, 0, len(productAgg))
	for _, row := range productAgg {
		resp.ProductsSold = append(resp.ProductsSold, *row)
		if row.SKU != "" {
			skus = append(skus, row.SKU)
		}
	}
	sort.Slice(resp.ProductsSold, func(i, j int) bool {
		return resp.ProductsSold[i].TotalAmount > resp.ProductsSold[j].TotalAmount
	})

	// Products by brand — resolve sku → brand via inventory (best-effort; missing = Unbranded).
	brandBySKU := map[string]string{}
	if h.inventory != nil && len(skus) > 0 {
		if resolved, berr := h.inventory.GetBrandsBySKU(r.Context(), tid.String(), skus); berr == nil {
			brandBySKU = resolved
		} else {
			h.log.Warn("register-details brand resolution failed", zap.Error(berr))
		}
	}
	brandAgg := map[string]*brandSoldRow{}
	for _, row := range resp.ProductsSold {
		brand := brandBySKU[row.SKU]
		if brand == "" {
			brand = "Unbranded"
		}
		b, ok := brandAgg[brand]
		if !ok {
			b = &brandSoldRow{Brand: brand}
			brandAgg[brand] = b
		}
		b.Quantity += row.Quantity
		b.TotalAmount += row.TotalAmount
	}
	for _, b := range brandAgg {
		resp.ProductsByBrand = append(resp.ProductsByBrand, *b)
	}
	sort.Slice(resp.ProductsByBrand, func(i, j int) bool {
		return resp.ProductsByBrand[i].TotalAmount > resp.ProductsByBrand[j].TotalAmount
	})

	resp.GrandTotal = resp.TotalSales + resp.ShippingTotal
	jsonOK(w, resp)
}
