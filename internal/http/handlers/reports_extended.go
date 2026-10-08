package handlers

import (
	"net/http"
	"sort"

	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entkdsstation "github.com/bengobox/pos-service/internal/ent/kdsstation"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/predicate"
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

	from, to := parseDateRange(r, requestTenantLocation(r, h.db))

	q := h.db.POSOrder.Query().
		Where(
			posorder.TenantID(tid),
			posorder.StatusEQ("voided"),
			posorder.CreatedAtGTE(from),
			posorder.CreatedAtLTE(to),
		)
	if outletFilter := parseOutletFilter(r); outletFilter != uuid.Nil {
		q = q.Where(posorder.OutletID(outletFilter))
	}
	orders, err := q.All(r.Context())
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
	names := resolveStaffNames(r.Context(), h.db, h.log, tid, ids)
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
// Returns two breakdowns: by order_subtype, and by item — the latter also carries each item's
// category and KDS station (resolved the same way computeKDSStationBreakdown does) so the
// Product Mix tab can filter by category/station instead of only free-text search.
// Permission required: pos.reports.view
func (h *ReportsHandler) ProductMix(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	from, to := parseDateRange(r, requestTenantLocation(r, h.db))

	orderPredicates := []predicate.POSOrder{
		posorder.TenantID(tid),
		posorder.StatusEQ("completed"),
		effectiveDateGTE(from),
		effectiveDateLTE(to),
	}
	if outletFilter := parseOutletFilter(r); outletFilter != uuid.Nil {
		orderPredicates = append(orderPredicates, posorder.OutletID(outletFilter))
	}

	type mixRow struct {
		Label       string  `json:"label"`
		Quantity    float64 `json:"quantity"`
		Revenue     float64 `json:"revenue"`
		OrderCount  int     `json:"order_count"`
		Category    string  `json:"category,omitempty"`
		StationName string  `json:"station_name,omitempty"`
		StationType string  `json:"station_type,omitempty"`
		// lastOrder lets OrderCount count distinct orders without a per-row set of order ids:
		// orders are scanned one at a time, so a row counts an order the first time it sees it.
		lastOrder uuid.UUID
	}
	bySubtype := make(map[string]*mixRow)
	byItem := make(map[string]*mixRow)
	byCategory := make(map[string]*mixRow)
	byStation := make(map[string]*mixRow)
	add := func(m map[string]*mixRow, key string, seed mixRow, orderID uuid.UUID, qty, revenue float64) {
		row, ok := m[key]
		if !ok {
			s := seed
			row = &s
			m[key] = row
		}
		if row.lastOrder != orderID {
			row.lastOrder = orderID
			row.OrderCount++
		}
		row.Quantity += qty
		row.Revenue += revenue
	}

	// Every station of the tenant in one query (same as computeKDSStationBreakdown): lines stamped
	// with a station that was later switched off still report under its real name, and a
	// multi-outlet report resolves each outlet's legacy unstamped lines against its own stations.
	allStations, err := h.db.KDSStation.Query().Where(entkdsstation.TenantID(tid)).All(r.Context())
	if err != nil {
		h.log.Error("product-mix stations query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	stationByID := make(map[uuid.UUID]*ent.KDSStation, len(allStations))
	for _, st := range allStations {
		stationByID[st.ID] = st
	}
	routerFor := outletRouters(allStations)
	resolveStation := func(o *ent.POSOrder, l *ent.POSOrderLine) (name, stype string) {
		stationID := l.KdsStationID
		if stationID == nil {
			stationID = routerFor(o.OutletID).OwnerOrFallback(l.Name, l.Category, nil)
		}
		if stationID == nil {
			return "", ""
		}
		if st := stationByID[*stationID]; st != nil {
			return st.Name, string(st.StationType)
		}
		return "", ""
	}

	// Orders-with-lines (not lines-with-order) so AttributeOrderLines can prorate each order's
	// net total_amount across its own lines, matching computeKDSStationBreakdown/SalesByCategory.
	// Read in keyset pages so a long range on a busy tenant never loads every order at once.
	var after *uuid.UUID
	for {
		q := h.db.POSOrder.Query().Where(orderPredicates...)
		if after != nil {
			q = q.Where(posorder.IDGT(*after))
		}
		mixOrders, err := q.Order(ent.Asc(posorder.FieldID)).Limit(kdsBreakdownPage).WithLines().All(r.Context())
		if err != nil {
			h.log.Error("product-mix query failed", zap.Error(err))
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, o := range mixOrders {
			// AttributeOrderLines fixes the same two bugs found in KDS-station/category: a
			// partially/fully voided line no longer contributes its pre-void gross, and revenue is
			// each line's prorated share of order.TotalAmount (net of discount/tax/charges/round-off)
			// rather than raw total_price — so every sub-breakdown here now agrees with Sales-by-Staff.
			attributed := AttributeOrderLines(o)
			for i, l := range o.Edges.Lines {
				al := attributed[i]
				// A fully-voided line contributes nothing active — skip it entirely rather than
				// still crediting its (zero) order-count membership across every dimension.
				if al.Quantity <= 0 && al.Revenue <= 0 {
					continue
				}

				subtype := string(o.OrderSubtype)
				add(bySubtype, subtype, mixRow{Label: subtype}, o.ID, al.Quantity, al.Revenue)

				stationName, stationType := resolveStation(o, l)
				category := l.Category
				if category == "" {
					category = "Uncategorised"
				}
				stationLabel := stationName
				if stationLabel == "" {
					stationLabel = "Unassigned"
				}

				add(byItem, l.Name, mixRow{Label: l.Name, Category: category, StationName: stationName, StationType: stationType}, o.ID, al.Quantity, al.Revenue)
				add(byCategory, category, mixRow{Label: category}, o.ID, al.Quantity, al.Revenue)
				add(byStation, stationLabel, mixRow{Label: stationLabel, StationName: stationName, StationType: stationType}, o.ID, al.Quantity, al.Revenue)
			}
		}
		if len(mixOrders) < kdsBreakdownPage {
			break
		}
		last := mixOrders[len(mixOrders)-1].ID
		after = &last
	}

	toSlice := func(m map[string]*mixRow) []*mixRow {
		s := make([]*mixRow, 0, len(m))
		for _, v := range m {
			s = append(s, v)
		}
		sort.Slice(s, func(i, j int) bool { return s[i].Revenue > s[j].Revenue })
		return s
	}

	jsonOK(w, map[string]any{
		"from":        from.Format("2006-01-02"),
		"to":          to.Format("2006-01-02"),
		"by_subtype":  toSlice(bySubtype),
		"top_items":   toSlice(byItem),
		"by_category": toSlice(byCategory),
		"by_station":  toSlice(byStation),
	})
}
