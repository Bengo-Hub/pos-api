package handlers

import (
	"net/http"

	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entkdsstation "github.com/bengobox/pos-service/internal/ent/kdsstation"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/modules/orders"
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

	from, to := parseDateRange(r)

	orderPredicates := []predicate.POSOrder{
		posorder.TenantID(tid),
		posorder.StatusEQ("completed"),
		effectiveDateGTE(from),
		effectiveDateLTE(to),
	}
	if outletFilter := parseOutletFilter(r); outletFilter != uuid.Nil {
		orderPredicates = append(orderPredicates, posorder.OutletID(outletFilter))
	}

	lines, err := h.db.POSOrderLine.Query().
		Where(posorderline.HasOrderWith(orderPredicates...)).
		WithOrder().
		All(r.Context())
	if err != nil {
		h.log.Error("product-mix query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	type mixRow struct {
		Label       string  `json:"label"`
		Quantity    float64 `json:"quantity"`
		Revenue     float64 `json:"revenue"`
		OrderCount  int     `json:"order_count"`
		Category    string  `json:"category,omitempty"`
		StationName string  `json:"station_name,omitempty"`
		StationType string  `json:"station_type,omitempty"`
	}
	bySubtype := make(map[string]*mixRow)
	byItem := make(map[string]*mixRow)
	byCategory := make(map[string]*mixRow)
	byStation := make(map[string]*mixRow)
	subtypeOrders := make(map[string]map[uuid.UUID]struct{})
	itemOrders := make(map[string]map[uuid.UUID]struct{})
	categoryOrders := make(map[string]map[uuid.UUID]struct{})
	stationOrders := make(map[string]map[uuid.UUID]struct{})

	// KDS station lookup, same per-outlet cache pattern as computeKDSStationBreakdown — a
	// multi-outlet report can mix outlets with different station configs.
	stationsByOutlet := make(map[uuid.UUID][]*ent.KDSStation)
	stationByID := make(map[uuid.UUID]*ent.KDSStation)
	resolveStation := func(o *ent.POSOrder, l *ent.POSOrderLine) (name, stype string) {
		stations, ok := stationsByOutlet[o.OutletID]
		if !ok {
			stations, _ = h.db.KDSStation.Query().
				Where(entkdsstation.TenantID(tid), entkdsstation.OutletID(o.OutletID), entkdsstation.IsActive(true)).
				All(r.Context())
			stationsByOutlet[o.OutletID] = stations
			for _, st := range stations {
				stationByID[st.ID] = st
			}
		}
		stationID := l.KdsStationID
		if stationID == nil {
			stationID = orders.ResolveStationForLineOrFallback(l.Name, l.Category, nil, stations)
		}
		if stationID == nil {
			return "", ""
		}
		if st := stationByID[*stationID]; st != nil {
			return st.Name, string(st.StationType)
		}
		return "", ""
	}

	for _, l := range lines {
		o := l.Edges.Order
		if o == nil {
			continue
		}
		subtype := string(o.OrderSubtype)
		if _, ok := bySubtype[subtype]; !ok {
			bySubtype[subtype] = &mixRow{Label: subtype}
		}
		if _, ok := subtypeOrders[subtype]; !ok {
			subtypeOrders[subtype] = map[uuid.UUID]struct{}{}
		}
		subtypeOrders[subtype][o.ID] = struct{}{}
		bySubtype[subtype].Revenue += l.TotalPrice
		bySubtype[subtype].Quantity += l.Quantity

		stationName, stationType := resolveStation(o, l)
		category := l.Category
		if category == "" {
			category = "Uncategorised"
		}
		stationLabel := stationName
		if stationLabel == "" {
			stationLabel = "Unassigned"
		}

		row, ok := byItem[l.Name]
		if !ok {
			row = &mixRow{Label: l.Name, Category: category, StationName: stationName, StationType: stationType}
			byItem[l.Name] = row
		}
		if _, ok := itemOrders[l.Name]; !ok {
			itemOrders[l.Name] = map[uuid.UUID]struct{}{}
		}
		itemOrders[l.Name][o.ID] = struct{}{}
		row.Quantity += l.Quantity
		row.Revenue += l.TotalPrice

		catRow, ok := byCategory[category]
		if !ok {
			catRow = &mixRow{Label: category}
			byCategory[category] = catRow
		}
		if _, ok := categoryOrders[category]; !ok {
			categoryOrders[category] = map[uuid.UUID]struct{}{}
		}
		categoryOrders[category][o.ID] = struct{}{}
		catRow.Quantity += l.Quantity
		catRow.Revenue += l.TotalPrice

		stRow, ok := byStation[stationLabel]
		if !ok {
			stRow = &mixRow{Label: stationLabel, StationName: stationName, StationType: stationType}
			byStation[stationLabel] = stRow
		}
		if _, ok := stationOrders[stationLabel]; !ok {
			stationOrders[stationLabel] = map[uuid.UUID]struct{}{}
		}
		stationOrders[stationLabel][o.ID] = struct{}{}
		stRow.Quantity += l.Quantity
		stRow.Revenue += l.TotalPrice
	}
	for subtype, ids := range subtypeOrders {
		bySubtype[subtype].OrderCount = len(ids)
	}
	for name, ids := range itemOrders {
		byItem[name].OrderCount = len(ids)
	}
	for cat, ids := range categoryOrders {
		byCategory[cat].OrderCount = len(ids)
	}
	for st, ids := range stationOrders {
		byStation[st].OrderCount = len(ids)
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
		"from":        from.Format("2006-01-02"),
		"to":          to.Format("2006-01-02"),
		"by_subtype":  toSlice(bySubtype),
		"top_items":   toSlice(byItem),
		"by_category": toSlice(byCategory),
		"by_station":  toSlice(byStation),
	})
}
