package handlers

import (
	"net/http"

	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/google/uuid"
)

// VoidSummary handles GET /{tenantID}/pos/reports/void-summary?from=&to=
// Groups voided orders by voided_by staff ID for fraud/abuse detection.
// Permission required: pos.reports.view
func (h *ReportsHandler) VoidSummary(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	from, to := parseDateRange(r)

	orders, err := h.db.POSOrder.Query().
		Where(
			posorder.TenantID(tid),
			posorder.StatusEQ("voided"),
			posorder.CreatedAtGTE(from),
			posorder.CreatedAtLTE(to),
		).
		All(r.Context())
	if err != nil {
		h.log.Error("void-summary query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	type voidBucket struct {
		VoidedBy    uuid.UUID      `json:"voided_by"`
		StaffName   string         `json:"staff_name"`
		VoidCount   int            `json:"void_count"`
		TotalAmount float64        `json:"total_voided_amount"`
		Reasons     map[string]int `json:"reasons"`
	}
	buckets := make(map[uuid.UUID]*voidBucket)
	unattributed := uuid.Nil

	for _, o := range orders {
		staffID := unattributed
		if o.VoidedBy != nil {
			staffID = *o.VoidedBy
		}
		if _, ok := buckets[staffID]; !ok {
			buckets[staffID] = &voidBucket{VoidedBy: staffID, Reasons: make(map[string]int)}
		}
		buckets[staffID].VoidCount++
		buckets[staffID].TotalAmount += o.TotalAmount
		reason := "unspecified"
		if o.VoidedReason != nil && *o.VoidedReason != "" {
			reason = *o.VoidedReason
		}
		buckets[staffID].Reasons[reason]++
	}

	// Enrich with staff names so the UI shows names, not UUIDs.
	ids := make([]uuid.UUID, 0, len(buckets))
	for id := range buckets {
		ids = append(ids, id)
	}
	names := h.resolveStaffNames(r.Context(), tid, ids)
	for id, b := range buckets {
		if id == unattributed {
			b.StaffName = "Unattributed"
		} else if n := names[id]; n != "" {
			b.StaffName = n
		} else {
			b.StaffName = "Unknown"
		}
	}

	rows := make([]*voidBucket, 0, len(buckets))
	for _, b := range buckets {
		rows = append(rows, b)
	}
	for i := 0; i < len(rows)-1; i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[j].VoidCount > rows[i].VoidCount {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}

	jsonOK(w, map[string]any{
		"from":  from.Format("2006-01-02"),
		"to":    to.Format("2006-01-02"),
		"items": rows,
	})
}

// ProductMix handles GET /{tenantID}/pos/reports/product-mix?from=&to=
// Returns three breakdowns: by category, by order_subtype, by item.
// Permission required: pos.reports.view
func (h *ReportsHandler) ProductMix(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	from, to := parseDateRange(r)

	lines, err := h.db.POSOrderLine.Query().
		Where(posorderline.HasOrderWith(
			posorder.TenantID(tid),
			posorder.StatusEQ("completed"),
			posorder.CreatedAtGTE(from),
			posorder.CreatedAtLTE(to),
		)).
		WithOrder().
		All(r.Context())
	if err != nil {
		h.log.Error("product-mix query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	type mixRow struct {
		Label      string  `json:"label"`
		Quantity   float64 `json:"quantity"`
		Revenue    float64 `json:"revenue"`
		OrderCount int     `json:"order_count"`
	}
	bySubtype := make(map[string]*mixRow)
	byItem := make(map[string]*mixRow)
	seenOrders := make(map[uuid.UUID]map[string]bool)

	for _, l := range lines {
		if l.Edges.Order != nil {
			subtype := string(l.Edges.Order.OrderSubtype)
			if _, ok := bySubtype[subtype]; !ok {
				bySubtype[subtype] = &mixRow{Label: subtype}
			}
			if _, ok := seenOrders[l.Edges.Order.ID]; !ok {
				seenOrders[l.Edges.Order.ID] = make(map[string]bool)
			}
			if !seenOrders[l.Edges.Order.ID][subtype] {
				seenOrders[l.Edges.Order.ID][subtype] = true
				bySubtype[subtype].OrderCount++
			}
			bySubtype[subtype].Revenue += l.TotalPrice
			bySubtype[subtype].Quantity += l.Quantity
		}

		if _, ok := byItem[l.Name]; !ok {
			byItem[l.Name] = &mixRow{Label: l.Name}
		}
		byItem[l.Name].Quantity += l.Quantity
		byItem[l.Name].Revenue += l.TotalPrice
	}

	toSlice := func(m map[string]*mixRow) []*mixRow {
		s := make([]*mixRow, 0, len(m))
		for _, v := range m {
			s = append(s, v)
		}
		for i := 0; i < len(s)-1; i++ {
			for j := i + 1; j < len(s); j++ {
				if s[j].Revenue > s[i].Revenue {
					s[i], s[j] = s[j], s[i]
				}
			}
		}
		return s
	}

	jsonOK(w, map[string]any{
		"from":       from.Format("2006-01-02"),
		"to":         to.Format("2006-01-02"),
		"by_subtype": toSlice(bySubtype),
		"top_items":  toSlice(byItem),
	})
}
